package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
	"github.com/maddeth/boozie-bot/internal/spotify"
)

// SpotifyHandler exposes the OAuth flow and now-playing endpoint.
type SpotifyHandler struct {
	tokens   *spotify.TokenManager
	svc      *services.SpotifyService
	auth     *auth.Middleware
	hasCreds bool

	stateMu      sync.Mutex
	pendingOAuth map[string]time.Time
}

// NewSpotifyHandler creates the handler. hasCreds indicates whether Spotify is
// configured at all (false → endpoints respond with 503 instead of crashing).
func NewSpotifyHandler(tokens *spotify.TokenManager, svc *services.SpotifyService, authMW *auth.Middleware, hasCreds bool) *SpotifyHandler {
	return &SpotifyHandler{
		tokens:       tokens,
		svc:          svc,
		auth:         authMW,
		hasCreds:     hasCreds,
		pendingOAuth: make(map[string]time.Time),
	}
}

// Register attaches Spotify routes to the mux.
func (h *SpotifyHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/spotify/status", h.status)
	mux.HandleFunc("GET /api/spotify/now-playing", h.nowPlaying)
	mux.HandleFunc("GET /api/spotify/callback", h.callback)
	mux.Handle("GET /api/spotify/auth", h.auth.AuthenticateToken(h.auth.RequireAdminRole(http.HandlerFunc(h.startAuth))))
	mux.Handle("GET /api/spotify/settings", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.getSettings))))
	mux.Handle("PUT /api/spotify/settings", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.updateSettings))))
}

// status reports whether Spotify is configured and whether the broadcaster has authorized.
func (h *SpotifyHandler) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": h.hasCreds,
		"authorized": h.hasCreds && h.tokens != nil && h.tokens.IsAuthorized(),
	})
}

// nowPlaying serves the cached now-playing state so the overlay can bootstrap
// without waiting for the next WebSocket broadcast.
func (h *SpotifyHandler) nowPlaying(w http.ResponseWriter, _ *http.Request) {
	if !h.hasCreds || h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "Spotify not configured")
		return
	}
	writeJSON(w, http.StatusOK, h.svc.Latest())
}

// getSettings returns the current runtime toggle state for the moderator UI.
func (h *SpotifyHandler) getSettings(w http.ResponseWriter, _ *http.Request) {
	if !h.hasCreds || h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "Spotify not configured")
		return
	}
	writeJSON(w, http.StatusOK, h.svc.Settings().Snapshot())
}

// updateSettings accepts a partial PATCH of either toggle. Fields are pointers
// so omitting one leaves it unchanged.
func (h *SpotifyHandler) updateSettings(w http.ResponseWriter, r *http.Request) {
	if !h.hasCreds || h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "Spotify not configured")
		return
	}

	var body struct {
		SongRequestsEnabled *bool `json:"songRequestsEnabled"`
		OverlayEnabled      *bool `json:"overlayEnabled"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	settings := h.svc.Settings()
	if body.SongRequestsEnabled != nil {
		if err := settings.SetSongRequestsEnabled(*body.SongRequestsEnabled); err != nil {
			logAndError(w, "Failed to persist songRequestsEnabled", err)
			return
		}
	}
	overlayChanged := false
	if body.OverlayEnabled != nil {
		if err := settings.SetOverlayEnabled(*body.OverlayEnabled); err != nil {
			logAndError(w, "Failed to persist overlayEnabled", err)
			return
		}
		overlayChanged = true
	}

	// Wake the polling loop so the overlay reflects the new state immediately
	// instead of waiting for the next idle tick (up to 30s).
	if overlayChanged {
		h.svc.Wake()
	}

	writeJSON(w, http.StatusOK, settings.Snapshot())
}

// startAuth (admin-only) returns the Spotify consent URL for the frontend to navigate to.
func (h *SpotifyHandler) startAuth(w http.ResponseWriter, _ *http.Request) {
	if !h.hasCreds || h.tokens == nil {
		writeError(w, http.StatusServiceUnavailable, "Spotify not configured")
		return
	}

	state, err := randomState()
	if err != nil {
		logAndError(w, "Failed to generate state", err)
		return
	}

	h.rememberState(state)
	writeJSON(w, http.StatusOK, map[string]string{
		"authorizeUrl": h.tokens.AuthorizeURL(state),
	})
}

// callback completes the OAuth flow. Spotify redirects the browser here with
// ?code=... and ?state=...; on success we render a tiny HTML page telling the
// broadcaster they can close the tab.
func (h *SpotifyHandler) callback(w http.ResponseWriter, r *http.Request) {
	if !h.hasCreds || h.tokens == nil {
		http.Error(w, "Spotify not configured", http.StatusServiceUnavailable)
		return
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		http.Error(w, "Spotify authorization denied: "+errParam, http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "Missing code or state parameter", http.StatusBadRequest)
		return
	}
	if !h.consumeState(state) {
		http.Error(w, "Invalid or expired state - restart the authorization flow", http.StatusBadRequest)
		return
	}

	if err := h.tokens.HandleCallback(code); err != nil {
		logAndError(w, "Spotify token exchange failed", err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta charset=utf-8><title>Spotify connected</title>
<style>body{font-family:system-ui,sans-serif;background:#0f0f10;color:#eee;display:grid;place-items:center;height:100vh;margin:0}
.card{background:#191920;padding:2rem 3rem;border-radius:.75rem;border:1px solid #2c2c36;text-align:center}
h1{margin:0 0 .5rem;color:#1db954}</style>
<div class=card><h1>Spotify connected</h1><p>You can close this tab.</p></div>`)
}

// --- state CSRF storage (in-memory, 10min TTL, single use) ---

const stateTTL = 10 * time.Minute

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (h *SpotifyHandler) rememberState(state string) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	now := time.Now()
	h.pendingOAuth[state] = now

	// Opportunistic cleanup of expired entries.
	for s, t := range h.pendingOAuth {
		if now.Sub(t) > stateTTL {
			delete(h.pendingOAuth, s)
		}
	}
}

func (h *SpotifyHandler) consumeState(state string) bool {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	t, ok := h.pendingOAuth[state]
	if !ok {
		return false
	}
	delete(h.pendingOAuth, state)
	return time.Since(t) <= stateTTL
}
