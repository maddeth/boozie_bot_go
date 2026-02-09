package twitch

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/maddeth/boozie-bot/internal/services"
)

const (
	headerMessageID        = "Twitch-Eventsub-Message-Id"
	headerMessageTimestamp  = "Twitch-Eventsub-Message-Timestamp"
	headerMessageSignature  = "Twitch-Eventsub-Message-Signature"
	headerMessageType       = "Twitch-Eventsub-Message-Type"
	headerSubscriptionType  = "Twitch-Eventsub-Subscription-Type"
)

var hexRegex = regexp.MustCompile(`^[0-9A-Fa-f]{6}$`)

// EventSubHandler handles Twitch EventSub webhook notifications.
type EventSubHandler struct {
	secret    string // webhook secret for HMAC verification
	eggSvc    *services.EggService
	alertSvc  *services.AlertService
	colourSvc *services.ColourService
	obsSvc    *services.OBSService
	sendChat  func(string) // sends a message to chat
	broadcast func(any)    // broadcasts to WebSocket clients
	myChannel string

	seenMu  sync.Mutex
	seenIDs map[string]time.Time // message ID dedup to prevent retries
}

// NewEventSubHandler creates a new EventSub webhook handler.
func NewEventSubHandler(
	secret string,
	eggSvc *services.EggService,
	alertSvc *services.AlertService,
	colourSvc *services.ColourService,
	obsSvc *services.OBSService,
	sendChat func(string),
	broadcast func(any),
	myChannel string,
) *EventSubHandler {
	return &EventSubHandler{
		secret:    secret,
		eggSvc:    eggSvc,
		alertSvc:  alertSvc,
		colourSvc: colourSvc,
		obsSvc:    obsSvc,
		sendChat:  sendChat,
		broadcast: broadcast,
		myChannel: myChannel,
		seenIDs:   make(map[string]time.Time),
	}
}

// CleanupSeenIDs removes message IDs older than 15 minutes.
// Call this periodically (e.g. from the periodic task ticker).
func (h *EventSubHandler) CleanupSeenIDs() {
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	cutoff := time.Now().Add(-15 * time.Minute)
	for id, t := range h.seenIDs {
		if t.Before(cutoff) {
			delete(h.seenIDs, id)
		}
	}
}

// VerifySignature checks the HMAC-SHA256 signature of an EventSub webhook request.
func (h *EventSubHandler) VerifySignature(messageID, timestamp string, body []byte, signature string) bool {
	message := messageID + timestamp + string(body)
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write([]byte(message))
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// HandleWebhook is the HTTP handler for EventSub webhook notifications.
func (h *EventSubHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	messageID := r.Header.Get(headerMessageID)
	timestamp := r.Header.Get(headerMessageTimestamp)
	signature := r.Header.Get(headerMessageSignature)
	messageType := r.Header.Get(headerMessageType)

	// Verify signature
	if !h.VerifySignature(messageID, timestamp, body, signature) {
		slog.Warn("eventsub signature verification failed", "message_id", messageID)
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	// Replay protection: reject messages older than 10 minutes
	ts, err := time.Parse(time.RFC3339, timestamp)
	if err == nil && time.Since(ts) > 10*time.Minute {
		http.Error(w, "message too old", http.StatusForbidden)
		return
	}

	switch messageType {
	case "webhook_callback_verification":
		var challenge struct {
			Challenge string `json:"challenge"`
		}
		json.Unmarshal(body, &challenge)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(challenge.Challenge))
		slog.Info("eventsub webhook verified", "message_id", messageID)

	case "notification":
		// Dedup: skip if we've already seen this message ID
		h.seenMu.Lock()
		if _, seen := h.seenIDs[messageID]; seen {
			h.seenMu.Unlock()
			slog.Warn("duplicate eventsub notification skipped", "message_id", messageID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.seenIDs[messageID] = time.Now()
		h.seenMu.Unlock()

		// Respond immediately so Twitch doesn't retry (3s timeout)
		w.WriteHeader(http.StatusNoContent)

		// Process the event asynchronously
		subType := r.Header.Get(headerSubscriptionType)
		bodyCopy := make([]byte, len(body))
		copy(bodyCopy, body)
		go h.handleNotification(bodyCopy, subType)

	case "revocation":
		slog.Warn("eventsub subscription revoked", "message_id", messageID)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

type eventSubPayload struct {
	Subscription struct {
		Type string `json:"type"`
	} `json:"subscription"`
	Event json.RawMessage `json:"event"`
}

func (h *EventSubHandler) handleNotification(body []byte, subType string) {
	var payload eventSubPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		slog.Error("failed to parse eventsub notification", "error", err)
		return
	}

	ctx := context.Background()

	switch subType {
	case "channel.subscribe":
		h.handleSubscription(ctx, payload.Event)
	case "channel.subscription.message":
		h.handleResubscription(ctx, payload.Event)
	case "channel.subscription.gift":
		h.handleGiftSubscription(ctx, payload.Event)
	case "channel.follow":
		h.handleFollow(ctx, payload.Event)
	case "channel.channel_points_custom_reward_redemption.add":
		h.handleRedemption(ctx, payload.Event)
	default:
		slog.Debug("unhandled eventsub type", "type", subType)
	}
}

func (h *EventSubHandler) handleSubscription(ctx context.Context, eventData json.RawMessage) {
	var event struct {
		UserID    string `json:"user_id"`
		UserLogin string `json:"user_login"`
		UserName  string `json:"user_name"`
		Tier      string `json:"tier"`
	}
	if err := json.Unmarshal(eventData, &event); err != nil {
		slog.Error("failed to parse subscription event", "error", err)
		return
	}

	eggReward := tierToBaseReward(event.Tier)
	_, err := h.eggSvc.EggUpdateCommand(ctx, event.UserLogin, eggReward, event.UserID)
	if err != nil {
		slog.Error("sub egg reward failed", "error", err, "user", event.UserLogin)
		return
	}

	h.sendChat(fmt.Sprintf("Thank you for subscribing %s! You've received %d eggs! 🥚✨", event.UserName, eggReward))
	slog.Info("subscription egg reward", "user", event.UserLogin, "tier", event.Tier, "reward", eggReward)
}

func (h *EventSubHandler) handleResubscription(ctx context.Context, eventData json.RawMessage) {
	var event struct {
		UserID           string `json:"user_id"`
		UserLogin        string `json:"user_login"`
		UserName         string `json:"user_name"`
		Tier             string `json:"tier"`
		CumulativeMonths int    `json:"cumulative_months"`
		StreakMonths     int    `json:"streak_months"`
	}
	if err := json.Unmarshal(eventData, &event); err != nil {
		slog.Error("failed to parse resub event", "error", err)
		return
	}

	baseReward := tierToBaseReward(event.Tier)
	loyaltyBonus := int(math.Min(float64(event.CumulativeMonths*10), 500))
	totalReward := baseReward + loyaltyBonus

	_, err := h.eggSvc.EggUpdateCommand(ctx, event.UserLogin, totalReward, event.UserID)
	if err != nil {
		slog.Error("resub egg reward failed", "error", err, "user", event.UserLogin)
		return
	}

	if event.StreakMonths > 0 {
		h.sendChat(fmt.Sprintf("%s resubscribed for %d months (%d month streak)! You've received %d eggs (%d base + %d loyalty bonus)! 🥚🔥",
			event.UserName, event.CumulativeMonths, event.StreakMonths, totalReward, baseReward, loyaltyBonus))
	} else {
		h.sendChat(fmt.Sprintf("%s resubscribed for %d months! You've received %d eggs (%d base + %d loyalty bonus)! 🥚✨",
			event.UserName, event.CumulativeMonths, totalReward, baseReward, loyaltyBonus))
	}

	slog.Info("resub egg reward", "user", event.UserLogin, "tier", event.Tier,
		"months", event.CumulativeMonths, "reward", totalReward)
}

func (h *EventSubHandler) handleGiftSubscription(ctx context.Context, eventData json.RawMessage) {
	var event struct {
		UserID      string `json:"user_id"`
		UserLogin   string `json:"user_login"`
		UserName    string `json:"user_name"`
		Tier        string `json:"tier"`
		Total       int    `json:"total"`
		IsAnonymous bool   `json:"is_anonymous"`
	}
	if err := json.Unmarshal(eventData, &event); err != nil {
		slog.Error("failed to parse gift sub event", "error", err)
		return
	}

	baseReward := tierToBaseReward(event.Tier)
	gifterReward := baseReward * event.Total

	if !event.IsAnonymous && event.UserID != "" {
		_, err := h.eggSvc.EggUpdateCommand(ctx, event.UserLogin, gifterReward, event.UserID)
		if err != nil {
			slog.Error("gift sub egg reward failed", "error", err, "user", event.UserLogin)
		}
		h.sendChat(fmt.Sprintf("%s gifted %d sub(s)! %s got %d eggs for their generosity! 🎁🥚",
			event.UserName, event.Total, event.UserName, gifterReward))
	} else {
		h.sendChat(fmt.Sprintf("An anonymous gifter gifted %d sub(s)! 🎁🥚", event.Total))
	}

	slog.Info("gift sub egg reward", "gifter", event.UserLogin, "total", event.Total, "reward", gifterReward)
}

func (h *EventSubHandler) handleFollow(ctx context.Context, eventData json.RawMessage) {
	var event struct {
		UserID    string `json:"user_id"`
		UserLogin string `json:"user_login"`
		UserName  string `json:"user_name"`
	}
	if err := json.Unmarshal(eventData, &event); err != nil {
		slog.Error("failed to parse follow event", "error", err)
		return
	}

	eggReward := 500
	_, err := h.eggSvc.EggUpdateCommand(ctx, event.UserLogin, eggReward, event.UserID)
	if err != nil {
		slog.Error("follow egg reward failed", "error", err, "user", event.UserLogin)
		return
	}

	h.sendChat(fmt.Sprintf("Welcome %s! Thanks for following! You've received %d eggs! 🥚💜", event.UserName, eggReward))
	slog.Info("follow egg reward", "user", event.UserLogin, "reward", eggReward)
}

func (h *EventSubHandler) handleRedemption(ctx context.Context, eventData json.RawMessage) {
	var event struct {
		UserID    string `json:"user_id"`
		UserLogin string `json:"user_login"`
		UserName  string `json:"user_name"`
		Reward    struct {
			Title string `json:"title"`
		} `json:"reward"`
		UserInput string `json:"user_input"`
	}
	if err := json.Unmarshal(eventData, &event); err != nil {
		slog.Error("failed to parse redemption event", "error", err)
		return
	}

	title := event.Reward.Title

	// Handle egg conversion redemptions
	switch title {
	case "Convert Feed to 100 Eggs":
		h.eggSvc.EggUpdateCommand(ctx, event.UserLogin, 100, event.UserID)
		slog.Info("egg conversion", "user", event.UserLogin, "amount", 100)
		return
	case "Convert Feed to 2000 Eggs":
		h.eggSvc.EggUpdateCommand(ctx, event.UserLogin, 2000, event.UserID)
		slog.Info("egg conversion", "user", event.UserLogin, "amount", 2000)
		return
	}

	// Check alert database
	alert, err := h.alertSvc.GetAlert(ctx, title)
	if err != nil {
		slog.Error("failed to get alert config", "error", err, "title", title)
		return
	}

	if alert != nil {
		redeem := map[string]any{
			"type":     "redeem",
			"audioUrl": alert.Audio,
		}
		if alert.GifURL != "" {
			redeem["gifUrl"] = alert.GifURL
		}
		if alert.Duration > 0 {
			redeem["duration"] = alert.Duration
		}
		h.broadcast(redeem)

		if title == "Shadow Colour" {
			h.handleColourChange(ctx, event.UserInput, event.UserLogin, event.UserID)
		}

		slog.Info("redemption processed", "title", title, "user", event.UserLogin)
	} else {
		slog.Warn("unknown redemption", "title", title, "user", event.UserLogin)
	}
}

// handleColourChange processes the "Shadow Colour" channel point redemption.
func (h *EventSubHandler) handleColourChange(ctx context.Context, userInput, userLogin, userID string) {
	if h.colourSvc == nil || h.obsSvc == nil {
		slog.Warn("colour change skipped: colour or OBS service not configured")
		return
	}

	colourString := strings.ToLower(strings.ReplaceAll(userInput, "#", ""))

	// Handle "random" requests
	if strings.HasPrefix(strings.TrimSpace(colourString), "random") {
		requested := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(colourString), "random"))
		randomColour, err := h.colourSvc.GetRandomByName(ctx, requested)
		if err == nil && randomColour != "" {
			hexVal, err := h.colourSvc.GetHexByName(ctx, randomColour)
			if err == nil && hexVal != "" {
				h.sendChat("Your Random Colour is " + randomColour)
				h.eggSvc.EggUpdateCommand(ctx, userLogin, 4, userID)
				h.obsSvc.ChangeColour(ctx, hexVal)
				slog.Info("random colour applied", "user", userLogin, "colour", randomColour, "hex", hexVal)
				return
			}
		}
	}

	// Handle "or" selection (e.g., "red or blue")
	if strings.Contains(strings.TrimSpace(colourString), " or ") {
		options := strings.Split(colourString, " or ")
		// Expand comma-separated within each option
		var allOptions []string
		for _, opt := range options {
			for _, sub := range strings.Split(opt, ",") {
				if trimmed := strings.TrimSpace(sub); trimmed != "" {
					allOptions = append(allOptions, trimmed)
				}
			}
		}
		if len(allOptions) > 0 {
			randBytes := make([]byte, 1)
			rand.Read(randBytes)
			selected := allOptions[int(randBytes[0])%len(allOptions)]
			hexVal, err := h.colourSvc.GetHexByName(ctx, selected)
			if err == nil && hexVal != "" {
				h.sendChat("Your Selected Colour is " + selected)
				h.eggSvc.EggUpdateCommand(ctx, userLogin, 4, userID)
				h.obsSvc.ChangeColour(ctx, hexVal)
				slog.Info("selected colour from options", "user", userLogin, "colour", selected)
				return
			}
		}
	}

	// Handle direct hex input
	if hexRegex.MatchString(strings.TrimSpace(colourString)) {
		hexVal := strings.TrimSpace(colourString)
		h.obsSvc.ChangeColour(ctx, hexVal)
		colourNames, _ := h.colourSvc.GetByHex(ctx, strings.ToUpper(hexVal))
		if len(colourNames) > 0 {
			h.sendChat("According to my list, that colour is " + strings.Join(colourNames, ", "))
		}
		h.eggSvc.EggUpdateCommand(ctx, userLogin, 4, userID)
		slog.Info("hex colour applied", "user", userLogin, "hex", hexVal)
		return
	}

	// Handle named colour from database
	hexVal, err := h.colourSvc.GetHexByName(ctx, colourString)
	if err == nil && hexVal != "" {
		h.sendChat("That colour is on my list! Congratulations, Here are 4 eggs!")
		h.eggSvc.EggUpdateCommand(ctx, userLogin, 4, userID)
		h.obsSvc.ChangeColour(ctx, hexVal)
		slog.Info("named colour applied", "user", userLogin, "colour", colourString, "hex", hexVal)
		return
	}

	// Fallback: random colour
	randBytes := make([]byte, 3)
	rand.Read(randBytes)
	randomHex := hex.EncodeToString(randBytes)
	colourNames, _ := h.colourSvc.GetByHex(ctx, strings.ToUpper(randomHex))
	if len(colourNames) > 0 {
		h.sendChat("That colour isn't in my list. You missed out on eggs Sadge here is a random colour instead: Hex: " + randomHex + " Colours: " + strings.Join(colourNames, ", "))
	} else {
		h.sendChat("That colour isn't in my list. You missed out on eggs Sadge here is a random colour instead: " + randomHex)
	}
	h.obsSvc.ChangeColour(ctx, randomHex)
	slog.Info("random fallback colour applied", "user", userLogin, "requested", colourString, "applied", randomHex)
}

// tierToBaseReward converts a Twitch sub tier to egg reward amount.
func tierToBaseReward(tier string) int {
	switch tier {
	case "2000":
		return 200
	case "3000":
		return 300
	default:
		return 100
	}
}
