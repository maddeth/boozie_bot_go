package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/bot"
	"github.com/maddeth/boozie-bot/internal/config"
	"github.com/maddeth/boozie-bot/internal/database"
	"github.com/maddeth/boozie-bot/internal/handlers"
	"github.com/maddeth/boozie-bot/internal/services"
	"github.com/maddeth/boozie-bot/internal/twitch"
	ws "github.com/maddeth/boozie-bot/internal/websocket"
)

// DistributionResult holds stats from the last egg distribution cycle.
type DistributionResult struct {
	Time                 time.Time      `json:"time"`
	StreamLive           bool           `json:"streamLive"`
	UniqueChatters       int            `json:"uniqueChatters"`
	Rewarded             int            `json:"rewarded"`
	TotalDistributed     int            `json:"totalDistributed"`
	TierCounts           map[string]int `json:"tierCounts"`
	SubLookupErrors      int            `json:"subLookupErrors"`
	UserCreateErrors     int            `json:"userCreateErrors"`
	RewardErrors         int            `json:"rewardErrors"`
}

var (
	lastDistributionMu     sync.RWMutex
	lastDistributionResult *DistributionResult
)

func main() {
	// Set up structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Load config
	cfg, err := config.Load("config.json")
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	slog.Info("config loaded", "channel", cfg.MyChannel, "port", cfg.Port,
		"pointsName", cfg.PointsName)

	// Initialize database
	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL environment variable is required")
		os.Exit(1)
	}

	db, err := database.New(ctx, databaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Initialize JWT verifier
	jwtVerifier, err := auth.NewJWTVerifier()
	if err != nil {
		slog.Error("failed to initialize JWT verifier", "error", err)
		os.Exit(1)
	}

	// Initialize auth middleware
	authMW := auth.NewMiddleware(jwtVerifier, db.Pool)

	// --- Token Manager & Twitch API ---
	tokenMgr := twitch.NewTokenManager(cfg.ClientID, cfg.ClientSecret)
	if err := tokenMgr.LoadTokenFile(cfg.BoozieBotUserID); err != nil {
		slog.Error("failed to load bot token", "error", err)
		os.Exit(1)
	}
	if err := tokenMgr.LoadTokenFile(cfg.MyChannelUserID); err != nil {
		slog.Error("failed to load streamer token", "error", err)
		os.Exit(1)
	}

	helixClient := twitch.NewHelixClient(tokenMgr, cfg.MyChannelUserID, cfg.BoozieBotUserID)

	// --- Core Services ---
	userSvc := services.NewUserService(db.Pool)
	eggSvc := services.NewEggService(db.Pool)
	commandSvc := services.NewCommandService(db.Pool, eggSvc, cfg.PointsName)
	quoteSvc := services.NewQuoteService(db.Pool)
	colourSvc := services.NewColourService(db.Pool)
	poolSvc := services.NewPoolService(db.Pool)
	alertSvc := services.NewAlertService(db.Pool)
	userMergeSvc := services.NewUserMergeService(db.Pool)
	shoutoutSvc := services.NewShoutoutService(db.Pool)

	// --- Supporting Services ---
	emoteSvc := services.NewEmoteService(cfg.MyChannelUserID, cfg.ClientID, func() (string, error) {
		return tokenMgr.GetAccessToken(cfg.BoozieBotUserID)
	})

	var obsSvc *services.OBSService
	if cfg.OBSIP != "" {
		obsSvc = services.NewOBSService(cfg.OBSIP, cfg.OBSPassword)
		slog.Info("OBS service configured", "address", cfg.OBSIP)
	}
	_ = obsSvc // available for future use in bot commands

	ttsSvc := services.NewTTSService(cfg.TTSDirectory)
	_ = ttsSvc // available for TTS command integration

	// --- TwitchSyncClient adapter for ModeratorSyncService ---
	syncAdapter := &helixSyncAdapter{helix: helixClient}
	modSyncSvc := services.NewModeratorSyncService(userSvc, syncAdapter, cfg.MyChannelUserID)

	// --- WebSocket Server ---
	wsServer := ws.New(cfg.WebSocketPort)
	wsCtx, wsCancel := context.WithCancel(context.Background())
	go func() {
		if err := wsServer.Start(wsCtx); err != nil {
			slog.Error("WebSocket server error", "error", err)
		}
	}()

	// --- IRC Chat Client ---
	// Resolve bot username from config or Helix API
	botUsername := cfg.Username
	if botUsername == "" {
		botUser, err := helixClient.GetUserByID(ctx, cfg.BoozieBotUserID)
		if err != nil || botUser == nil {
			slog.Error("failed to resolve bot username from Twitch API", "error", err)
			os.Exit(1)
		}
		botUsername = botUser.Login
		slog.Info("resolved bot username from Twitch API", "username", botUsername)
	}
	botTokenFunc := func() (string, error) {
		return tokenMgr.GetAccessToken(cfg.BoozieBotUserID)
	}
	chatClient := twitch.NewChatClient(botUsername, botTokenFunc, cfg.MyChannel, cfg.BoozieBotUserID)

	// --- Bot (command router) ---
	botInstance := bot.New(cfg, chatClient, helixClient, wsServer,
		userSvc, eggSvc, commandSvc, quoteSvc, poolSvc, shoutoutSvc, userMergeSvc, alertSvc, emoteSvc)
	chatClient.OnMessage(botInstance.HandleMessage)

	go func() {
		if err := chatClient.Connect(); err != nil {
			slog.Error("IRC connection error", "error", err)
		}
	}()

	// --- EventSub Handler ---
	eventSubHandler := twitch.NewEventSubHandler(
		cfg.Secret, eggSvc, alertSvc, colourSvc, obsSvc,
		chatClient.Say, wsServer.Broadcast, cfg.MyChannel,
		cfg.PointsName, cfg.PointsNameSingular, cfg.PointsEmoji,
	)

	// --- Load Initial Data ---
	if err := commandSvc.LoadCommands(ctx); err != nil {
		slog.Error("failed to load commands", "error", err)
	}
	if err := shoutoutSvc.LoadAutoShoutoutList(ctx); err != nil {
		slog.Error("failed to load auto-shoutout list", "error", err)
	}
	if err := emoteSvc.LoadAllEmotes(ctx); err != nil {
		slog.Error("failed to load emotes", "error", err)
	} else {
		// Broadcast emotes to WebSocket clients (cached for late joiners)
		allEmotes := emoteSvc.GetAllEmotes()
		wsServer.BroadcastImmediate(map[string]any{
			"type":   "emotes",
			"emotes": allEmotes,
		})
	}

	// Check initial stream status
	if live, err := helixClient.IsStreamLive(ctx); err == nil {
		if live {
			slog.Info("stream status check", "status", "online")
		} else {
			slog.Info("stream status check", "status", "offline")
		}
	}

	// --- Set up HTTP server ---
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		stats := db.Stats()
		wsStats := wsServer.Stats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"db": map[string]any{
				"acquired_conns": stats.AcquiredConns(),
				"idle_conns":     stats.IdleConns(),
				"total_conns":    stats.TotalConns(),
			},
			"websocket": map[string]any{
				"connections": wsStats.TotalConnections,
				"unique_ips":  wsStats.UniqueIPs,
			},
		})
	})

	// Distribution debug endpoint
	interval := time.Duration(cfg.EggUpdateInterval) * time.Millisecond
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	mux.HandleFunc("GET /api/debug/distribution", func(w http.ResponseWriter, r *http.Request) {
		lastDistributionMu.RLock()
		result := lastDistributionResult
		lastDistributionMu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		if result == nil {
			json.NewEncoder(w).Encode(map[string]any{
				"message":  "No distribution has run yet",
				"interval": interval.String(),
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"lastDistribution": result,
			"interval":         interval.String(),
			"nextRunApprox":    result.Time.Add(interval).Format(time.RFC3339),
		})
	})

	// Register API handlers
	handlers.NewAlertHandler(alertSvc, authMW).Register(mux)
	handlers.NewEggHandler(eggSvc, userSvc, authMW).Register(mux)
	handlers.NewQuoteHandler(quoteSvc, authMW).Register(mux)
	handlers.NewColourHandler(colourSvc, authMW).Register(mux)
	handlers.NewPoolHandler(poolSvc, userSvc, authMW).Register(mux)
	handlers.NewCommandHandler(commandSvc, userSvc, db.Pool, authMW).Register(mux)
	handlers.NewUserMergeHandler(userMergeSvc, authMW).Register(mux)
	handlers.NewShoutoutHandler(shoutoutSvc, helixClient, db.Pool, authMW).Register(mux)
	handlers.NewUserRoleHandler(userSvc, db.Pool, authMW).Register(mux)
	handlers.NewWebhookHandler(eventSubHandler, tokenMgr, cfg).Register(mux)
	handlers.NewStaticHandler(cfg.TTSDirectory).Register(mux)

	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      handlers.CORSMiddleware(handlers.RequestLogger(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start HTTP server
	go func() {
		slog.Info("starting HTTP server", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// --- Start Periodic Tasks ---
	periodicCtx, periodicCancel := context.WithCancel(context.Background())
	go runPeriodicTasks(periodicCtx, cfg, helixClient, botInstance, userSvc, eggSvc, emoteSvc, modSyncSvc, wsServer, eventSubHandler)

	slog.Info("bot started",
		"port", cfg.Port,
		"wsPort", cfg.WebSocketPort,
		"channel", cfg.MyChannel,
	)

	// --- Graceful Shutdown ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	slog.Info("shutting down", "signal", sig.String())

	// 1. Stop periodic tasks
	periodicCancel()

	// 2. Disconnect IRC chat
	if err := chatClient.Disconnect(); err != nil {
		slog.Error("IRC disconnect error", "error", err)
	}

	// 3. Stop WebSocket server
	wsCancel()

	// 4. Shutdown HTTP server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}

	// 5. Database pool closed by deferred db.Close()

	slog.Info("server stopped")
}

// runPeriodicTasks runs distribution, mod/sub sync, TTS cleanup, and emote refresh
// on a ticker matching the eggUpdateInterval (default 15 minutes).
func runPeriodicTasks(
	ctx context.Context,
	cfg *config.Config,
	helix *twitch.HelixClient,
	botInstance *bot.Bot,
	userSvc *services.UserService,
	eggSvc *services.EggService,
	emoteSvc *services.EmoteService,
	modSyncSvc *services.ModeratorSyncService,
	wsServer *ws.Server,
	eventSub *twitch.EventSubHandler,
) {
	interval := time.Duration(cfg.EggUpdateInterval) * time.Millisecond
	if interval <= 0 {
		interval = 15 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("periodic tasks started", "interval", interval.String())

	for {
		select {
		case <-ctx.Done():
			slog.Info("periodic tasks stopped")
			return
		case <-ticker.C:
			runPeriodicTick(ctx, cfg, helix, botInstance, userSvc, eggSvc, emoteSvc, modSyncSvc, wsServer, eventSub)
		}
	}
}

func runPeriodicTick(
	ctx context.Context,
	cfg *config.Config,
	helix *twitch.HelixClient,
	botInstance *bot.Bot,
	userSvc *services.UserService,
	eggSvc *services.EggService,
	emoteSvc *services.EmoteService,
	modSyncSvc *services.ModeratorSyncService,
	wsServer *ws.Server,
	eventSub *twitch.EventSubHandler,
) {
	// EventSub message ID dedup cleanup
	eventSub.CleanupSeenIDs()

	// TTS file cleanup (remove .mp3 files older than 5 minutes)
	cleanupTTSFiles(cfg.TTSDirectory)

	// Check stream status
	live, err := helix.IsStreamLive(ctx)
	if err != nil {
		slog.Error("stream status check failed", "error", err)
	}

	if live {
		// Fetch chatters and update bot's chatter map
		chatters, err := helix.GetChatters(ctx)
		if err != nil {
			slog.Error("failed to fetch chatters", "error", err)
		} else {
			botInstance.UpdateChatters(chatters)

			// Distribute points to active chatters
			result := distributeEggs(ctx, chatters, helix, userSvc, eggSvc, cfg)
			result.StreamLive = true
			lastDistributionMu.Lock()
			lastDistributionResult = result
			lastDistributionMu.Unlock()

			// Sync subscriber status
			syncResult := modSyncSvc.SyncSubscribers(ctx, chatters)
			if syncResult.Success {
				slog.Info("subscriber sync completed",
					"synced", syncResult.Synced,
					"errors", syncResult.Errors,
				)
			}
		}
	} else {
		// Record that stream was offline
		lastDistributionMu.Lock()
		lastDistributionResult = &DistributionResult{Time: time.Now(), StreamLive: false}
		lastDistributionMu.Unlock()
	}

	// Sync moderators (always, regardless of stream status)
	modResult := modSyncSvc.SyncModerators(ctx)
	if !modResult.Success {
		slog.Error("moderator sync failed", "error", modResult.Error)
	}

	// Refresh emotes (has 1hr cache, will no-op if fresh)
	if err := emoteSvc.LoadAllEmotes(ctx); err != nil {
		slog.Error("emote refresh failed", "error", err)
	} else {
		allEmotes := emoteSvc.GetAllEmotes()
		wsServer.BroadcastImmediate(map[string]any{
			"type":   "emotes",
			"emotes": allEmotes,
		})
	}
}

// distributeEggs rewards chatters with points based on their subscription tier.
func distributeEggs(
	ctx context.Context,
	chatters map[string]string,
	helix *twitch.HelixClient,
	userSvc *services.UserService,
	eggSvc *services.EggService,
	cfg *config.Config,
) *DistributionResult {
	seen := make(map[string]bool, len(chatters))
	result := &DistributionResult{
		Time:       time.Now(),
		TierCounts: make(map[string]int),
	}

	slog.Info("periodic distribution starting", "totalChatters", len(chatters))

	for displayName, userID := range chatters {
		if seen[userID] {
			continue
		}
		seen[userID] = true

		tier, err := helix.GetSubscription(ctx, userID)
		if err != nil {
			slog.Warn("sub lookup failed during distribution", "user", displayName, "error", err)
			tier = "0" // Still reward them at base rate
			result.SubLookupErrors++
		}

		// Reward based on tier (Go GetSubscription returns "0", "1", "2", "3")
		var reward int
		switch tier {
		case "1":
			reward = 10
		case "2":
			reward = 15
		case "3":
			reward = 20
		default:
			reward = 5
		}
		result.TierCounts[tier]++

		// Ensure user exists in database
		dn := displayName
		_, err = userSvc.GetOrCreateUser(ctx, userID, strings.ToLower(displayName), &dn)
		if err != nil {
			slog.Warn("user creation failed during distribution", "user", displayName, "error", err)
			result.UserCreateErrors++
			continue
		}

		_, err = eggSvc.EggUpdateCommand(ctx, strings.ToLower(displayName), reward, userID, cfg.PointsName, cfg.PointsNameSingular)
		if err != nil {
			slog.Warn("reward failed", "user", displayName, "error", err)
			result.RewardErrors++
			continue
		}

		result.Rewarded++
		result.TotalDistributed += reward
	}

	result.UniqueChatters = len(seen)
	slog.Info("distribution completed",
		"uniqueChatters", result.UniqueChatters,
		"rewarded", result.Rewarded,
		"totalDistributed", result.TotalDistributed,
		"tiers", result.TierCounts,
		"subLookupErrors", result.SubLookupErrors,
		"userCreateErrors", result.UserCreateErrors,
		"rewardErrors", result.RewardErrors,
	)
	return result
}

// cleanupTTSFiles removes .mp3 files older than 5 minutes from the TTS directory.
func cleanupTTSFiles(directory string) {
	if directory == "" {
		return
	}

	cutoff := time.Now().Add(-5 * time.Minute)
	var removed int

	filepath.WalkDir(directory, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".mp3") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
		return nil
	})

	if removed > 0 {
		slog.Info("TTS cleanup", "removed", removed)
	}
}

// --- TwitchSyncClient adapter ---

// helixSyncAdapter adapts twitch.HelixClient to the services.TwitchSyncClient interface.
// This breaks the import cycle between services and twitch packages.
type helixSyncAdapter struct {
	helix *twitch.HelixClient
}

func (a *helixSyncAdapter) GetModeratorList(ctx context.Context) ([]services.TwitchModInfo, error) {
	mods, err := a.helix.GetModerators(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]services.TwitchModInfo, len(mods))
	for i, m := range mods {
		result[i] = services.TwitchModInfo{
			UserID:    m.UserID,
			UserLogin: m.UserLogin,
			UserName:  m.UserName,
		}
	}
	return result, nil
}

func (a *helixSyncAdapter) LookupUserByID(ctx context.Context, userID string) (login, displayName string, err error) {
	user, err := a.helix.GetUserByID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if user == nil {
		return "", "", nil
	}
	return user.Login, user.DisplayName, nil
}

func (a *helixSyncAdapter) GetSubTier(ctx context.Context, userID string) (string, error) {
	return a.helix.GetSubscription(ctx, userID)
}
