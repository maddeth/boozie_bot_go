package handlers

import (
	"net/http"
	"strings"

	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
)

// AlertHandler handles /api/alerts/* endpoints.
type AlertHandler struct {
	alerts *services.AlertService
	auth   *auth.Middleware
}

// NewAlertHandler creates a new alert handler.
func NewAlertHandler(alerts *services.AlertService, authMW *auth.Middleware) *AlertHandler {
	return &AlertHandler{alerts: alerts, auth: authMW}
}

// Register registers alert routes on the given mux.
func (h *AlertHandler) Register(mux *http.ServeMux) {
	// Public
	mux.HandleFunc("GET /api/alerts/", h.getAll)
	mux.HandleFunc("GET /api/alerts/{eventTitle}", h.getOne)

	// Moderator-protected
	mux.Handle("POST /api/alerts/", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.create))))
	mux.Handle("PUT /api/alerts/{eventTitle}", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.update))))
	mux.Handle("DELETE /api/alerts/{eventTitle}", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.delete))))
}

func (h *AlertHandler) getAll(w http.ResponseWriter, r *http.Request) {
	alerts, err := h.alerts.GetAllAlerts(r.Context())
	if err != nil {
		logAndError(w, "Failed to fetch alerts", err)
		return
	}
	writeJSON(w, http.StatusOK, alerts)
}

func (h *AlertHandler) getOne(w http.ResponseWriter, r *http.Request) {
	eventTitle := r.PathValue("eventTitle")
	alert, err := h.alerts.GetAlert(r.Context(), eventTitle)
	if err != nil {
		logAndError(w, "Failed to fetch alert", err)
		return
	}
	if alert == nil {
		writeError(w, http.StatusNotFound, "Alert not found")
		return
	}
	writeJSON(w, http.StatusOK, alert)
}

func (h *AlertHandler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EventTitle string `json:"event_title"`
		AudioURL   string `json:"audio_url"`
		GifURL     string `json:"gif_url"`
		DurationMS int    `json:"duration_ms"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if body.EventTitle == "" || body.AudioURL == "" {
		writeError(w, http.StatusBadRequest, "Event title and audio URL are required")
		return
	}

	alert, err := h.alerts.CreateAlert(r.Context(), body.EventTitle, body.AudioURL, body.GifURL, body.DurationMS)
	if err != nil {
		if strings.Contains(err.Error(), "23505") {
			writeError(w, http.StatusConflict, "Alert with this event title already exists")
			return
		}
		logAndError(w, "Failed to create alert", err)
		return
	}
	writeJSON(w, http.StatusCreated, alert)
}

func (h *AlertHandler) update(w http.ResponseWriter, r *http.Request) {
	eventTitle := r.PathValue("eventTitle")

	var body struct {
		AudioURL   *string `json:"audio_url"`
		GifURL     *string `json:"gif_url"`
		DurationMS *int    `json:"duration_ms"`
		Enabled    *bool   `json:"enabled"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	alert, err := h.alerts.UpdateAlert(r.Context(), eventTitle, body.AudioURL, body.GifURL, body.DurationMS, body.Enabled)
	if err != nil {
		logAndError(w, "Failed to update alert", err)
		return
	}
	if alert == nil {
		writeError(w, http.StatusNotFound, "Alert not found")
		return
	}
	writeJSON(w, http.StatusOK, alert)
}

func (h *AlertHandler) delete(w http.ResponseWriter, r *http.Request) {
	eventTitle := r.PathValue("eventTitle")

	alert, err := h.alerts.DeleteAlert(r.Context(), eventTitle)
	if err != nil {
		logAndError(w, "Failed to delete alert", err)
		return
	}
	if alert == nil {
		writeError(w, http.StatusNotFound, "Alert not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Alert deleted successfully"})
}
