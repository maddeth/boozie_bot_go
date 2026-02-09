package handlers

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
	"github.com/maddeth/boozie-bot/internal/twitch"
)

// ShoutoutHandler handles /api/shoutouts/* endpoints.
type ShoutoutHandler struct {
	shoutouts *services.ShoutoutService
	helix     *twitch.HelixClient
	db        *pgxpool.Pool
	auth      *auth.Middleware
}

// NewShoutoutHandler creates a new shoutout handler.
func NewShoutoutHandler(shoutouts *services.ShoutoutService, helix *twitch.HelixClient, db *pgxpool.Pool, authMW *auth.Middleware) *ShoutoutHandler {
	return &ShoutoutHandler{shoutouts: shoutouts, helix: helix, db: db, auth: authMW}
}

// Register registers shoutout routes on the given mux.
func (h *ShoutoutHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/shoutouts/auto-shoutouts", h.getList)
	mux.HandleFunc("POST /api/shoutouts/auto-shoutouts", h.addUser)
	mux.HandleFunc("DELETE /api/shoutouts/auto-shoutouts/{userId}", h.removeUser)
}

// autoShoutoutRow matches the DB schema for auto_shoutouts.
type autoShoutoutRow struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	AddedAt     any    `json:"added_at"`
}

func (h *ShoutoutHandler) getList(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(),
		`SELECT user_id, username, display_name, added_at FROM auto_shoutouts ORDER BY added_at DESC`,
	)
	if err != nil {
		logAndError(w, "Failed to get auto-shoutout list", err)
		return
	}
	defer rows.Close()

	var users []autoShoutoutRow
	for rows.Next() {
		var u autoShoutoutRow
		if err := rows.Scan(&u.UserID, &u.Username, &u.DisplayName, &u.AddedAt); err != nil {
			logAndError(w, "Failed to get auto-shoutout list", err)
			return
		}
		users = append(users, u)
	}
	if users == nil {
		users = []autoShoutoutRow{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"users":   users,
	})
}

func (h *ShoutoutHandler) addUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
	}
	if err := readJSON(r, &body); err != nil || body.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "username is required",
		})
		return
	}

	// Look up user on Twitch
	if h.helix == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"error":   "Twitch API not available",
		})
		return
	}

	twitchUser, err := h.helix.GetUserByName(r.Context(), body.Username)
	if err != nil {
		logAndError(w, "Failed to look up Twitch user", err)
		return
	}
	if twitchUser == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false,
			"error":   "Twitch user \"" + body.Username + "\" not found",
		})
		return
	}

	// Upsert into auto_shoutouts
	_, err = h.db.Exec(r.Context(),
		`INSERT INTO auto_shoutouts (user_id, username, display_name)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_id) DO UPDATE SET username = $2, display_name = $3, added_at = NOW()`,
		twitchUser.ID, twitchUser.Login, twitchUser.DisplayName,
	)
	if err != nil {
		logAndError(w, "Failed to add user to auto-shoutout list", err)
		return
	}

	// Update in-memory list
	h.shoutouts.AddToAutoShoutoutList(twitchUser.ID)

	slog.Info("user added to auto-shoutout list", "user_id", twitchUser.ID, "username", twitchUser.Login)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": twitchUser.DisplayName + " added to auto-shoutout list",
	})
}

func (h *ShoutoutHandler) removeUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userId")

	_, err := h.db.Exec(r.Context(),
		`DELETE FROM auto_shoutouts WHERE user_id = $1`, userID,
	)
	if err != nil {
		logAndError(w, "Failed to remove user from auto-shoutout list", err)
		return
	}

	h.shoutouts.RemoveFromAutoShoutoutList(userID)

	slog.Info("user removed from auto-shoutout list", "user_id", userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "User removed from auto-shoutout list",
	})
}
