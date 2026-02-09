package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/maddeth/boozie-bot/internal/config"
	"github.com/maddeth/boozie-bot/internal/twitch"
)

// WebhookHandler handles EventSub webhook routes.
type WebhookHandler struct {
	eventsub *twitch.EventSubHandler
	tokenMgr *twitch.TokenManager
	cfg      *config.Config
}

// NewWebhookHandler creates a new webhook handler.
func NewWebhookHandler(eventsub *twitch.EventSubHandler, tokenMgr *twitch.TokenManager, cfg *config.Config) *WebhookHandler {
	return &WebhookHandler{
		eventsub: eventsub,
		tokenMgr: tokenMgr,
		cfg:      cfg,
	}
}

// Register registers webhook routes on the given mux.
func (h *WebhookHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /notification", h.eventsub.HandleWebhook)
	mux.HandleFunc("POST /createWebhook/{broadcasterId}", h.createWebhooks)
}

// createWebhooks creates EventSub subscriptions for all supported event types.
func (h *WebhookHandler) createWebhooks(w http.ResponseWriter, r *http.Request) {
	broadcasterID := r.PathValue("broadcasterId")
	if broadcasterID == "" {
		writeError(w, http.StatusBadRequest, "broadcasterId is required")
		return
	}

	eventTypes := []struct {
		name    string
		version string
	}{
		{"channel.channel_points_custom_reward_redemption.add", "1"},
		{"channel.subscribe", "1"},
		{"channel.subscription.message", "1"},
		{"channel.subscription.gift", "1"},
		{"channel.follow", "2"},
	}

	slog.Info("creating webhooks for all event types", "broadcasterId", broadcasterID, "count", len(eventTypes))

	token, err := h.tokenMgr.GetAppAccessToken()
	if err != nil {
		logAndError(w, "Failed to get app access token for webhook creation", err)
		return
	}

	type result struct {
		EventType string `json:"eventType"`
		Success   bool   `json:"success"`
		Data      any    `json:"data,omitempty"`
		Error     string `json:"error,omitempty"`
	}

	var results []result
	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, et := range eventTypes {
		condition := map[string]string{
			"broadcaster_user_id": broadcasterID,
		}
		if et.name == "channel.follow" {
			condition["moderator_user_id"] = h.cfg.BoozieBotUserID
		}

		body := map[string]any{
			"type":      et.name,
			"version":   et.version,
			"condition": condition,
			"transport": map[string]string{
				"method":   "webhook",
				"callback": h.cfg.WebAddress + "/notification",
				"secret":   h.cfg.Secret,
			},
		}

		bodyJSON, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			"https://api.twitch.tv/helix/eventsub/subscriptions", bytes.NewReader(bodyJSON))
		if err != nil {
			results = append(results, result{EventType: et.name, Success: false, Error: err.Error()})
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Client-ID", h.cfg.ClientID)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			results = append(results, result{EventType: et.name, Success: false, Error: err.Error()})
			slog.Error("webhook creation request failed", "eventType", et.name, "error", err)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var data any
			json.Unmarshal(respBody, &data)
			results = append(results, result{EventType: et.name, Success: true, Data: data})
			slog.Info("webhook created successfully", "eventType", et.name, "broadcasterId", broadcasterID)
		} else {
			errMsg := fmt.Sprintf("status %d: %s", resp.StatusCode, respBody)
			results = append(results, result{EventType: et.name, Success: false, Error: errMsg})
			slog.Error("webhook creation failed", "eventType", et.name, "broadcasterId", broadcasterID, "status", resp.StatusCode)
		}
	}

	successful := 0
	for _, r := range results {
		if r.Success {
			successful++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"broadcasterId": broadcasterID,
		"results":       results,
		"summary": map[string]int{
			"total":      len(eventTypes),
			"successful": successful,
			"failed":     len(eventTypes) - successful,
		},
	})
}
