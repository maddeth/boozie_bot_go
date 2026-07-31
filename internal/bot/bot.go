package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/maddeth/boozie-bot/internal/config"
	"github.com/maddeth/boozie-bot/internal/services"
	"github.com/maddeth/boozie-bot/internal/twitch"
)

// Broadcaster sends messages to connected WebSocket clients.
// Implemented by the WebSocket server (Phase 5). Use NopBroadcaster until then.
type Broadcaster interface {
	Broadcast(msg any)
	BroadcastImmediate(msg any)
}

// NopBroadcaster is a no-op broadcaster for use before WebSocket is ready.
type NopBroadcaster struct{}

func (NopBroadcaster) Broadcast(any)          {}
func (NopBroadcaster) BroadcastImmediate(any) {}

// Bot is the chat command router. It receives messages from the IRC client
// and dispatches them to the appropriate command handlers.
type Bot struct {
	cfg   *config.Config
	chat  *twitch.ChatClient
	helix *twitch.HelixClient
	ws    Broadcaster

	users      *services.UserService
	eggs       *services.EggService
	commands   *services.CommandService
	quotes     *services.QuoteService
	pools      *services.PoolService
	shoutouts  *services.ShoutoutService
	merge      *services.UserMergeService
	alerts     *services.AlertService
	emotes     *services.EmoteService
	spotifySvc *services.SpotifyService // nil when Spotify is disabled

	// chatters maps displayName/username -> twitchUserID for user resolution.
	chatters sync.Map

	// !sr cooldowns (only allocated when Spotify is enabled).
	srUserCooldown  *cooldownMap // per-user rate limit
	srTrackCooldown *cooldownMap // per-track duplicate-prevention window

	// Precomputed command prefixes from config (e.g. "!eggs", "!addeggs")
	cmdPoints      string
	cmdAddPoints   string
	cmdTopPoints   string
	cmdMergePoints string
}

// New creates a new Bot with all its service dependencies.
func New(
	cfg *config.Config,
	chat *twitch.ChatClient,
	helix *twitch.HelixClient,
	ws Broadcaster,
	users *services.UserService,
	eggs *services.EggService,
	commands *services.CommandService,
	quotes *services.QuoteService,
	pools *services.PoolService,
	shoutouts *services.ShoutoutService,
	merge *services.UserMergeService,
	alerts *services.AlertService,
	emotes *services.EmoteService,
	spotifySvc *services.SpotifyService,
) *Bot {
	if ws == nil {
		ws = NopBroadcaster{}
	}
	b := &Bot{
		cfg:            cfg,
		chat:           chat,
		helix:          helix,
		ws:             ws,
		users:          users,
		eggs:           eggs,
		commands:       commands,
		quotes:         quotes,
		pools:          pools,
		shoutouts:      shoutouts,
		merge:          merge,
		alerts:         alerts,
		emotes:         emotes,
		spotifySvc:     spotifySvc,
		cmdPoints:      "!" + cfg.PointsName,
		cmdAddPoints:   "!add" + cfg.PointsName,
		cmdTopPoints:   "!top" + cfg.PointsName,
		cmdMergePoints: "!merge" + cfg.PointsName,
	}
	if spotifySvc != nil {
		b.srUserCooldown = newCooldownMap()
		b.srTrackCooldown = newCooldownMap()
	}
	return b
}

// HandleMessage is the entry point called by the chat client for each incoming message.
func (b *Bot) HandleMessage(msg *twitch.ChatMessage) {
	ctx := context.Background()
	displayName := msg.User.DisplayName
	lowerText := strings.ToLower(msg.Text)

	// Broadcast chat message to WebSocket clients (for chat view)
	var parsedMessage any = msg.Text
	if b.emotes != nil {
		parsedMessage = b.emotes.ParseMessage(msg.Text)
	}
	b.ws.Broadcast(map[string]any{
		"type":          "chat",
		"user":          displayName,
		"message":       msg.Text,
		"parsedMessage": parsedMessage,
		"isMod":         msg.User.IsMod,
		"isVip":         msg.User.IsVIP,
		"isSubscriber":  msg.User.IsSubscriber,
		"isBroadcaster": msg.User.IsBroadcaster,
		"color":         msg.User.Color,
		"timestamp":     msg.Time.Format("2006-01-02T15:04:05.000Z"),
	})

	// Ensure user exists in database
	if msg.User.ID != "" {
		dn := displayName
		_, err := b.users.GetOrCreateUser(ctx, msg.User.ID, msg.User.Name, &dn)
		if err != nil {
			slog.Error("failed to create user from chat", "error", err, "user", displayName)
		}
		b.chatters.Store(displayName, msg.User.ID)
		b.chatters.Store(strings.ToLower(displayName), msg.User.ID)
	}

	// Check for auto-shoutout
	if msg.User.ID != "" && b.shoutouts.ShouldAutoShoutout(msg.User.ID) {
		b.handleAutoShoutout(ctx, msg)
	}

	// Route commands (order matters - more specific prefixes first)
	switch {
	case strings.HasPrefix(lowerText, b.cmdAddPoints):
		b.cmdAddEggs(ctx, msg)
	case strings.HasPrefix(lowerText, b.cmdTopPoints):
		b.cmdTopEggs(ctx, msg)
	case strings.HasPrefix(lowerText, b.cmdPoints):
		b.cmdEggs(ctx, msg)
	case strings.HasPrefix(lowerText, "!reloadcommands"):
		b.cmdReloadCommands(ctx, msg)
	case strings.HasPrefix(lowerText, "!commands"):
		b.cmdListCommands(ctx, msg)
	case strings.HasPrefix(lowerText, "!so ") || strings.HasPrefix(lowerText, "!shoutout "):
		b.cmdShoutout(ctx, msg)
	case strings.HasPrefix(lowerText, "!donate "):
		b.cmdDonate(ctx, msg)
	case strings.HasPrefix(lowerText, "!createpool "):
		b.cmdCreatePool(ctx, msg)
	case strings.HasPrefix(lowerText, "!deletepool "):
		b.cmdDeletePool(ctx, msg)
	case strings.HasPrefix(lowerText, "!pools"):
		b.cmdListPools(ctx, msg)
	case strings.HasPrefix(lowerText, "!pool "):
		b.cmdPool(ctx, msg)
	case strings.HasPrefix(lowerText, b.cmdMergePoints+" "):
		b.cmdMergeEggs(ctx, msg)
	case strings.HasPrefix(lowerText, "!addquote ") || strings.HasPrefix(lowerText, "!quoteadd "):
		b.cmdAddQuote(ctx, msg)
	case strings.HasPrefix(lowerText, "!delquote "):
		b.cmdDelQuote(ctx, msg)
	case strings.HasPrefix(lowerText, "!quote"):
		b.cmdQuote(ctx, msg)
	case lowerText == "!sr" || strings.HasPrefix(lowerText, "!sr "):
		b.cmdSongRequest(ctx, msg)
	case lowerText == "!song":
		b.cmdSong(ctx, msg)
	case lowerText == "!songqueue" || lowerText == "!sq":
		b.cmdSongQueue(ctx, msg)
	default:
		// Fallthrough: check custom commands from database
		b.cmdCustom(ctx, msg)
	}
}

// say sends a message to the chat channel.
func (b *Bot) say(message string) {
	b.chat.Say(message)
}

// sayf sends a formatted message to the chat channel.
func (b *Bot) sayf(format string, args ...any) {
	b.chat.Say(fmt.Sprintf(format, args...))
}

// isMod checks if the message sender is a moderator (via badges or database).
func (b *Bot) isMod(ctx context.Context, msg *twitch.ChatMessage) bool {
	if msg.User.IsMod || msg.User.IsBroadcaster {
		return true
	}
	// Fallback to database
	user, err := b.users.GetByUsername(ctx, msg.User.Name)
	if err != nil {
		return false
	}
	return user != nil && (user.IsModerator || user.IsAdmin)
}

// getPermissions returns the user's chat permissions, preferring real-time badge data.
func (b *Bot) getPermissions(msg *twitch.ChatMessage) *services.ChatUserInfo {
	return &services.ChatUserInfo{
		Username:     msg.User.Name,
		DisplayName:  msg.User.DisplayName,
		TwitchUserID: msg.User.ID,
		IsModerator:  msg.User.IsMod || msg.User.IsBroadcaster,
		IsSubscriber: msg.User.IsSubscriber,
		IsVIP:        msg.User.IsVIP,
		Channel:      msg.Channel,
	}
}

// resolveTwitchUserID tries to resolve a username to a Twitch user ID.
// Checks: chatters map -> Helix API lookup.
func (b *Bot) resolveTwitchUserID(ctx context.Context, username string) string {
	if id, ok := b.chatters.Load(username); ok {
		return id.(string)
	}
	if id, ok := b.chatters.Load(strings.ToLower(username)); ok {
		return id.(string)
	}

	// Try Helix API
	user, err := b.helix.GetUserByName(ctx, strings.ToLower(username))
	if err != nil {
		slog.Debug("helix user lookup failed", "username", username, "error", err)
		return ""
	}
	if user == nil {
		return ""
	}

	// Cache for future lookups
	b.chatters.Store(username, user.ID)
	b.chatters.Store(user.Login, user.ID)
	return user.ID
}

// handleAutoShoutout sends an auto-shoutout if the user is on the list. On a
// rate limit the shoutout is queued for a later retry instead of being dropped.
func (b *Bot) handleAutoShoutout(ctx context.Context, msg *twitch.ChatMessage) {
	user, err := b.helix.GetUserByName(ctx, msg.User.Name)
	if err != nil || user == nil {
		slog.Error("auto-shoutout user lookup failed", "user", msg.User.Name, "error", err)
		return
	}

	p := services.PendingShoutout{
		UserID:      user.ID,
		DisplayName: msg.User.DisplayName,
		Login:       user.Login,
	}

	switch err := b.sendAutoShoutout(ctx, p); {
	case err == nil:
		slog.Info("auto-shoutout sent", "user", p.DisplayName)
	case errors.Is(err, twitch.ErrShoutoutRateLimited):
		b.shoutouts.QueuePendingShoutout(p)
		slog.Warn("auto-shoutout rate limited, queued for retry", "user", p.DisplayName)
	default:
		// Not marked done and not queued, so it retries on the user's next message.
		slog.Error("auto-shoutout failed", "user", p.DisplayName, "error", err)
	}
}

// sendAutoShoutout fires the native Twitch shoutout and, on success, marks the
// user done and posts the chat message. Returns the SendShoutout error (nil on
// success) so callers can decide whether to queue or retry. Shared by the
// initial attempt and the retry loop.
func (b *Bot) sendAutoShoutout(ctx context.Context, p services.PendingShoutout) error {
	if err := b.helix.SendShoutout(ctx, p.UserID); err != nil {
		return err
	}
	b.shoutouts.MarkShoutedOut(p.UserID)
	b.shoutouts.RemovePendingShoutout(p.UserID)
	b.sayf("Check out %s! Follow them at twitch.tv/%s", p.DisplayName, p.Login)
	return nil
}

// RetryPendingShoutouts attempts queued shoutouts that previously failed (e.g.
// were rate limited). Called periodically. Stops at the first rate limit so the
// remaining items wait for the next pass rather than burning failed attempts
// against the channel-wide 2-minute limit.
func (b *Bot) RetryPendingShoutouts(ctx context.Context) {
	pending := b.shoutouts.GetPendingShoutouts()
	if len(pending) == 0 {
		return
	}

	for _, p := range pending {
		// Skip if no longer eligible (already shouted out via a chat message
		// since this snapshot, or removed from the list mid-stream).
		if !b.shoutouts.ShouldAutoShoutout(p.UserID) {
			b.shoutouts.RemovePendingShoutout(p.UserID)
			continue
		}

		switch err := b.sendAutoShoutout(ctx, p); {
		case err == nil:
			b.shoutouts.RemovePendingShoutout(p.UserID)
			slog.Info("auto-shoutout sent on retry", "user", p.DisplayName)
		case errors.Is(err, twitch.ErrShoutoutRateLimited):
			slog.Info("auto-shoutout still rate limited, will retry next pass", "user", p.DisplayName)
			return
		default:
			b.shoutouts.RemovePendingShoutout(p.UserID)
			slog.Error("auto-shoutout retry failed, dropping from queue", "user", p.DisplayName, "error", err)
		}
	}
}

// ResetStreamShoutouts clears the per-stream auto-shoutout tracking so regulars
// become eligible again. Called by the periodic poll on the stream offline edge.
func (b *Bot) ResetStreamShoutouts() {
	b.shoutouts.ResetStreamShoutouts()
}

// UpdateChatters replaces the chatters map with fresh data from a Helix API fetch.
func (b *Bot) UpdateChatters(chatters map[string]string) {
	b.chatters.Range(func(key, _ any) bool {
		b.chatters.Delete(key)
		return true
	})
	for name, id := range chatters {
		b.chatters.Store(name, id)
	}
}
