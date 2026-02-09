package handlers

import (
	"math"
	"net/http"

	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
)

// EggHandler handles /api/eggs/* endpoints.
type EggHandler struct {
	eggs  *services.EggService
	users *services.UserService
	auth  *auth.Middleware
}

// NewEggHandler creates a new egg handler.
func NewEggHandler(eggs *services.EggService, users *services.UserService, authMW *auth.Middleware) *EggHandler {
	return &EggHandler{eggs: eggs, users: users, auth: authMW}
}

// Register registers egg routes on the given mux.
func (h *EggHandler) Register(mux *http.ServeMux) {
	// Auth-protected
	mux.Handle("GET /api/eggs/my-eggs", h.auth.AuthenticateToken(http.HandlerFunc(h.myEggs)))
	mux.Handle("GET /api/eggs/all", h.auth.AuthenticateToken(http.HandlerFunc(h.getAll)))

	// Public
	mux.HandleFunc("GET /api/eggs/leaderboard", h.leaderboard)
	mux.HandleFunc("GET /api/eggs/stats", h.stats)
	mux.HandleFunc("GET /api/eggs/user/{username}", h.userEggs)
}

func (h *EggHandler) myEggs(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetClaims(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	user, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil {
		logAndError(w, "Failed to retrieve user", err)
		return
	}
	if user == nil {
		writeError(w, http.StatusNotFound, "User not found. Please link your Twitch account.")
		return
	}
	if user.TwitchUserID == nil {
		writeError(w, http.StatusBadRequest, "No Twitch account linked. Please link your Twitch account to view eggs.")
		return
	}

	eggData, err := h.eggs.GetUserEggs(r.Context(), *user.TwitchUserID)
	if err != nil {
		logAndError(w, "Failed to retrieve egg data", err)
		return
	}

	if eggData == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"username":    user.Username,
			"displayName": user.DisplayName,
			"eggs":        0,
			"hasEggs":     false,
			"rank":        nil,
		})
		return
	}

	rank, err := h.eggs.GetUserRank(r.Context(), eggData.EggsAmount)
	if err != nil {
		logAndError(w, "Failed to calculate rank", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"username":    eggData.Username,
		"displayName": user.DisplayName,
		"eggs":        eggData.EggsAmount,
		"hasEggs":     true,
		"lastUpdated": eggData.UpdatedAt,
		"rank":        rank,
	})
}

func (h *EggHandler) leaderboard(w http.ResponseWriter, r *http.Request) {
	limit := parseIntParam(r, "limit", 10, 50)

	entries, err := h.eggs.GetLeaderboard(r.Context(), limit)
	if err != nil {
		logAndError(w, "Failed to retrieve leaderboard", err)
		return
	}

	type lbEntry struct {
		Rank        int    `json:"rank"`
		Username    string `json:"username"`
		Eggs        int    `json:"eggs"`
		LastUpdated any    `json:"lastUpdated"`
	}

	lb := make([]lbEntry, len(entries))
	for i, e := range entries {
		lb[i] = lbEntry{
			Rank:        i + 1,
			Username:    e.Username,
			Eggs:        e.EggsAmount,
			LastUpdated: e.UpdatedAt,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"leaderboard":    lb,
		"totalShown":     len(lb),
		"requestedLimit": limit,
	})
}

func (h *EggHandler) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.eggs.GetStats(r.Context())
	if err != nil {
		logAndError(w, "Failed to retrieve statistics", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"totalUsers":  stats.TotalUsers,
		"totalEggs":   stats.TotalEggs,
		"averageEggs": math.Round(stats.AverageEggs*100) / 100,
		"maxEggs":     stats.MaxEggs,
	})
}

func (h *EggHandler) userEggs(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if len(username) < 2 {
		writeError(w, http.StatusBadRequest, "Invalid username")
		return
	}

	eggData, err := h.eggs.GetUserEggs(r.Context(), username)
	if err != nil {
		logAndError(w, "Failed to retrieve user data", err)
		return
	}
	if eggData == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":    "User not found or has no eggs",
			"username": username,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"username":    eggData.Username,
		"eggs":        eggData.EggsAmount,
		"lastUpdated": eggData.UpdatedAt,
	})
}

func (h *EggHandler) getAll(w http.ResponseWriter, r *http.Request) {
	// Require moderator or admin
	claims := auth.GetClaims(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	user, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil {
		logAndError(w, "Failed to verify permissions", err)
		return
	}
	if user == nil || (!user.IsAdmin && !user.IsModerator) {
		writeError(w, http.StatusForbidden, "Admin privileges required")
		return
	}

	limit := parseIntParam(r, "limit", 100, 500)
	orderByUsername := r.URL.Query().Get("order") == "username"

	allUsers, err := h.eggs.GetAll(r.Context(), limit, orderByUsername)
	if err != nil {
		logAndError(w, "Failed to retrieve user data", err)
		return
	}

	type eggEntry struct {
		Username    string `json:"username"`
		Eggs        int    `json:"eggs"`
		TwitchUID   any    `json:"twitchUserId"`
		CreatedAt   any    `json:"createdAt"`
		UpdatedAt   any    `json:"updatedAt"`
	}

	users := make([]eggEntry, len(allUsers))
	for i, u := range allUsers {
		users[i] = eggEntry{
			Username:  u.Username,
			Eggs:      u.EggsAmount,
			TwitchUID: u.TwitchUserID,
			CreatedAt: u.CreatedAt,
			UpdatedAt: u.UpdatedAt,
		}
	}

	orderBy := "eggs"
	if orderByUsername {
		orderBy = "username"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"users":          users,
		"totalShown":     len(users),
		"orderBy":        orderBy,
		"requestedLimit": limit,
	})
}
