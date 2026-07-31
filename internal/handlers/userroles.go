package handlers

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
)

// UserRoleHandler handles /api/user/* endpoints.
type UserRoleHandler struct {
	users *services.UserService
	db    *pgxpool.Pool
	auth  *auth.Middleware
}

// NewUserRoleHandler creates a new user role handler.
func NewUserRoleHandler(users *services.UserService, db *pgxpool.Pool, authMW *auth.Middleware) *UserRoleHandler {
	return &UserRoleHandler{users: users, db: db, auth: authMW}
}

// Register registers user role routes on the given mux.
func (h *UserRoleHandler) Register(mux *http.ServeMux) {
	// Auth-protected
	mux.Handle("GET /api/user/me", h.auth.AuthenticateToken(http.HandlerFunc(h.me)))
	mux.Handle("GET /api/user/me/moderator", h.auth.AuthenticateToken(http.HandlerFunc(h.meModerator)))
	mux.Handle("POST /api/user/me/refresh", h.auth.AuthenticateToken(http.HandlerFunc(h.meRefresh)))
	mux.Handle("POST /api/user/link", h.auth.AuthenticateToken(http.HandlerFunc(h.linkAccount)))

	// Moderator-protected
	mux.Handle("GET /api/user/moderators", h.auth.AuthenticateToken(http.HandlerFunc(h.moderators)))
	mux.Handle("GET /api/user/admins", h.auth.AuthenticateToken(http.HandlerFunc(h.admins)))
	mux.Handle("GET /api/user/stats", h.auth.AuthenticateToken(http.HandlerFunc(h.stats)))
	mux.Handle("GET /api/user/stats/users", h.auth.AuthenticateToken(http.HandlerFunc(h.statsUsers)))
	mux.Handle("GET /api/user/stats/moderators", h.auth.AuthenticateToken(http.HandlerFunc(h.statsModerators)))
	mux.Handle("GET /api/user/stats/subscribers", h.auth.AuthenticateToken(http.HandlerFunc(h.statsSubscribers)))
	mux.Handle("GET /api/user/stats/registered", h.auth.AuthenticateToken(http.HandlerFunc(h.statsRegistered)))
	mux.Handle("GET /api/user/stats/active-weekly", h.auth.AuthenticateToken(http.HandlerFunc(h.statsActiveWeekly)))

	// Public
	mux.HandleFunc("GET /api/user/check/{twitchUserId}", h.checkModerator)

	// Superadmin-protected
	mux.Handle("PUT /api/user/admin/{username}", h.auth.AuthenticateToken(h.auth.RequireSuperAdminRole(http.HandlerFunc(h.updateAdmin))))
	mux.Handle("PUT /api/user/moderator/{username}", h.auth.AuthenticateToken(h.auth.RequireSuperAdminRole(http.HandlerFunc(h.updateModerator))))
}

func (h *UserRoleHandler) me(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetClaims(r.Context())
	user, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil {
		logAndError(w, "Failed to retrieve user information", err)
		return
	}
	if user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "User not found",
			"message": "No user record found. Please visit the stream to create your profile.",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"username":     user.Username,
		"displayName":  user.DisplayName,
		"twitchUserId": user.TwitchUserID,
		"roles": map[string]bool{
			"isModerator":  user.IsModerator,
			"isAdmin":      user.IsAdmin,
			"isSuperAdmin": user.IsSuperAdmin,
			"isSubscriber": user.IsSubscriber,
		},
		"subscriptionTier": user.SubscriptionTier,
		"moderatorSince":   user.ModeratorSince,
		"lastSeen":         user.LastSeen,
	})
}

func (h *UserRoleHandler) meModerator(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetClaims(r.Context())
	user, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":       "Internal server error",
			"isModerator": false,
		})
		return
	}
	if user == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"isModerator": false,
			"message":     "User not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"isModerator":    user.IsModerator,
		"moderatorSince": user.ModeratorSince,
	})
}

func (h *UserRoleHandler) meRefresh(w http.ResponseWriter, r *http.Request) {
	// This endpoint requires moderatorSyncService which is Phase 7.
	// For now, just return the current user info.
	claims := auth.GetClaims(r.Context())
	user, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil {
		logAndError(w, "Failed to refresh moderator status", err)
		return
	}
	if user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "User not found",
			"message": "No user record found. Please visit the stream to create your profile.",
		})
		return
	}

	// TODO: Wire moderatorSyncService in Phase 7 for actual Twitch sync
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Moderator status refreshed successfully",
		"roleInfo": map[string]any{
			"username":     user.Username,
			"displayName":  user.DisplayName,
			"twitchUserId": user.TwitchUserID,
			"roles": map[string]bool{
				"isModerator":  user.IsModerator,
				"isAdmin":      user.IsAdmin,
				"isSuperAdmin": user.IsSuperAdmin,
				"isSubscriber": user.IsSubscriber,
			},
			"subscriptionTier": user.SubscriptionTier,
			"moderatorSince":   user.ModeratorSince,
			"lastSeen":         user.LastSeen,
		},
	})
}

func (h *UserRoleHandler) moderators(w http.ResponseWriter, r *http.Request) {
	if !h.isModeratorRequest(w, r) {
		return
	}

	mods, err := h.users.GetModerators(r.Context())
	if err != nil {
		logAndError(w, "Failed to retrieve moderators list", err)
		return
	}

	type modInfo struct {
		Username       string `json:"username"`
		DisplayName    any    `json:"displayName"`
		ModeratorSince any    `json:"moderatorSince"`
		LastSeen       any    `json:"lastSeen"`
	}

	list := make([]modInfo, len(mods))
	for i, m := range mods {
		list[i] = modInfo{
			Username:       m.Username,
			DisplayName:    m.DisplayName,
			ModeratorSince: m.ModeratorSince,
			LastSeen:       m.LastSeen,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"moderators": list,
		"count":      len(list),
	})
}

func (h *UserRoleHandler) admins(w http.ResponseWriter, r *http.Request) {
	if !h.isModeratorRequest(w, r) {
		return
	}

	rows, err := h.db.Query(r.Context(),
		`SELECT username, display_name, is_moderator, is_admin, is_superadmin, last_seen
		 FROM users WHERE is_admin = true OR is_superadmin = true ORDER BY username ASC`,
	)
	if err != nil {
		logAndError(w, "Failed to retrieve bot admins list", err)
		return
	}
	defer rows.Close()

	type adminRow struct {
		Username     string `json:"username"`
		DisplayName  any    `json:"display_name"`
		IsModerator  bool   `json:"is_moderator"`
		IsAdmin      bool   `json:"is_admin"`
		IsSuperAdmin bool   `json:"is_superadmin"`
		LastSeen     any    `json:"last_seen"`
	}

	var admins []adminRow
	for rows.Next() {
		var a adminRow
		if err := rows.Scan(&a.Username, &a.DisplayName, &a.IsModerator, &a.IsAdmin, &a.IsSuperAdmin, &a.LastSeen); err != nil {
			logAndError(w, "Failed to retrieve bot admins list", err)
			return
		}
		admins = append(admins, a)
	}
	if admins == nil {
		admins = []adminRow{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"admins": admins,
		"count":  len(admins),
	})
}

func (h *UserRoleHandler) stats(w http.ResponseWriter, r *http.Request) {
	if !h.isModeratorRequest(w, r) {
		return
	}

	stats, err := h.users.GetUserStats(r.Context())
	if err != nil {
		logAndError(w, "Failed to retrieve user statistics", err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *UserRoleHandler) checkModerator(w http.ResponseWriter, r *http.Request) {
	twitchUserID := r.PathValue("twitchUserId")
	if twitchUserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "Bad request",
			"message": "Twitch user ID is required",
		})
		return
	}

	isMod, err := h.users.IsModerator(r.Context(), twitchUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":       "Internal server error",
			"isModerator": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"twitchUserId": twitchUserID,
		"isModerator":  isMod,
	})
}

func (h *UserRoleHandler) linkAccount(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetClaims(r.Context())

	var body struct {
		TwitchUsername string  `json:"twitchUsername"`
		Email          *string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil || body.TwitchUsername == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "Bad request",
			"message": "Twitch username is required",
		})
		return
	}

	twitchUser, err := h.users.GetByUsername(r.Context(), body.TwitchUsername)
	if err != nil {
		logAndError(w, "Failed to link accounts", err)
		return
	}
	if twitchUser == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "User not found",
			"message": "No Twitch user found with that username",
		})
		return
	}
	if twitchUser.TwitchUserID == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "Link failed",
			"message": "User has no Twitch ID associated",
		})
		return
	}

	if err := h.users.LinkSupabaseUser(r.Context(), *twitchUser.TwitchUserID, claims.Subject, body.Email); err != nil {
		logAndError(w, "Failed to link accounts", err)
		return
	}

	slog.Info("linked Supabase user to Twitch user",
		"twitch_username", body.TwitchUsername,
		"twitch_user_id", *twitchUser.TwitchUserID,
		"supabase_user_id", claims.Subject,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Accounts linked successfully",
		"user": map[string]any{
			"username":     twitchUser.Username,
			"twitchUserId": twitchUser.TwitchUserID,
			"isModerator":  twitchUser.IsModerator,
		},
	})
}

func (h *UserRoleHandler) updateAdmin(w http.ResponseWriter, r *http.Request) {
	// Require superadmin
	claims := auth.GetClaims(r.Context())
	requestingUser, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil || requestingUser == nil || !requestingUser.IsSuperAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "Forbidden",
			"message": "Superadmin privileges required",
		})
		return
	}

	username := r.PathValue("username")
	var body struct {
		IsAdmin bool `json:"isAdmin"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "Bad request",
			"message": "isAdmin must be a boolean value",
		})
		return
	}

	tag, err := h.db.Exec(r.Context(),
		`UPDATE users SET is_admin = $1 WHERE LOWER(username) = LOWER($2)`,
		body.IsAdmin, username,
	)
	if err != nil {
		logAndError(w, "Failed to update bot admin status", err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "User not found",
			"message": "User " + username + " not found in database",
		})
		return
	}

	slog.Info("bot admin status updated",
		"updated_by", requestingUser.Username,
		"target", username,
		"is_admin", body.IsAdmin,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": username + " bot admin status updated",
	})
}

// updateModerator promotes or demotes a user to/from bot moderator.
// Requires superadmin.
func (h *UserRoleHandler) updateModerator(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetClaims(r.Context())
	requestingUser, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil || requestingUser == nil || !requestingUser.IsSuperAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "Forbidden",
			"message": "Superadmin privileges required",
		})
		return
	}

	username := r.PathValue("username")
	var body struct {
		IsModerator bool `json:"isModerator"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "Bad request",
			"message": "isModerator must be a boolean value",
		})
		return
	}

	// Look up the user to get their Twitch ID for cache invalidation
	targetUser, err := h.users.GetByUsername(r.Context(), username)
	if err != nil {
		logAndError(w, "Failed to update moderator status", err)
		return
	}
	if targetUser == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "User not found",
			"message": "User " + username + " not found in database",
		})
		return
	}

	tag, err := h.db.Exec(r.Context(),
		`UPDATE users SET
			is_moderator = $1,
			moderator_updated = NOW(),
			moderator_since = CASE
				WHEN $1 = true AND moderator_since IS NULL THEN NOW()
				ELSE moderator_since
			END
		 WHERE LOWER(username) = LOWER($2)`,
		body.IsModerator, username,
	)
	if err != nil {
		logAndError(w, "Failed to update moderator status", err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "User not found",
			"message": "User " + username + " not found in database",
		})
		return
	}

	// Invalidate cache if we have the Twitch ID
	if targetUser.TwitchUserID != nil {
		h.users.InvalidateCacheByTwitchID(*targetUser.TwitchUserID)
	}

	slog.Info("moderator status updated",
		"updated_by", requestingUser.Username,
		"target", username,
		"is_moderator", body.IsModerator,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": username + " moderator status updated",
	})
}

// Stats sub-endpoints - all moderator-protected

func (h *UserRoleHandler) statsUsers(w http.ResponseWriter, r *http.Request) {
	if !h.isModeratorRequest(w, r) {
		return
	}
	h.queryUserStats(w, r,
		`SELECT username, display_name, is_moderator, is_admin, is_subscriber, subscription_tier, last_seen, created_at
		 FROM users ORDER BY last_seen DESC NULLS LAST LIMIT 100`)
}

func (h *UserRoleHandler) statsModerators(w http.ResponseWriter, r *http.Request) {
	if !h.isModeratorRequest(w, r) {
		return
	}
	h.queryUserStats(w, r,
		`SELECT username, display_name, is_moderator, is_admin, is_subscriber, subscription_tier, last_seen, created_at
		 FROM users WHERE is_moderator = true ORDER BY moderator_since ASC NULLS LAST`)
}

func (h *UserRoleHandler) statsSubscribers(w http.ResponseWriter, r *http.Request) {
	if !h.isModeratorRequest(w, r) {
		return
	}
	h.queryUserStats(w, r,
		`SELECT username, display_name, is_moderator, is_admin, is_subscriber, subscription_tier, last_seen, created_at
		 FROM users WHERE is_subscriber = true ORDER BY subscription_tier DESC, last_seen DESC`)
}

func (h *UserRoleHandler) statsRegistered(w http.ResponseWriter, r *http.Request) {
	if !h.isModeratorRequest(w, r) {
		return
	}
	h.queryUserStats(w, r,
		`SELECT username, display_name, is_moderator, is_admin, is_subscriber, subscription_tier, last_seen, created_at
		 FROM users WHERE supabase_user_id IS NOT NULL ORDER BY created_at DESC`)
}

func (h *UserRoleHandler) statsActiveWeekly(w http.ResponseWriter, r *http.Request) {
	if !h.isModeratorRequest(w, r) {
		return
	}
	h.queryUserStats(w, r,
		`SELECT username, display_name, is_moderator, is_admin, is_subscriber, subscription_tier, last_seen, created_at
		 FROM users WHERE last_seen > NOW() - INTERVAL '7 days' ORDER BY last_seen DESC`)
}

// queryUserStats is a helper that runs a query and returns the result as a user list.
func (h *UserRoleHandler) queryUserStats(w http.ResponseWriter, r *http.Request, query string) {
	rows, err := h.db.Query(r.Context(), query)
	if err != nil {
		logAndError(w, "Internal server error", err)
		return
	}
	defer rows.Close()

	type userRow struct {
		Username         string `json:"username"`
		DisplayName      any    `json:"display_name"`
		IsModerator      bool   `json:"is_moderator"`
		IsAdmin          bool   `json:"is_admin"`
		IsSubscriber     bool   `json:"is_subscriber"`
		SubscriptionTier any    `json:"subscription_tier"`
		LastSeen         any    `json:"last_seen"`
		CreatedAt        any    `json:"created_at"`
	}

	var users []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.Username, &u.DisplayName, &u.IsModerator, &u.IsAdmin,
			&u.IsSubscriber, &u.SubscriptionTier, &u.LastSeen, &u.CreatedAt); err != nil {
			logAndError(w, "Internal server error", err)
			return
		}
		users = append(users, u)
	}
	if users == nil {
		users = []userRow{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// isModeratorRequest checks if the authenticated user is a moderator, writing an error response if not.
func (h *UserRoleHandler) isModeratorRequest(w http.ResponseWriter, r *http.Request) bool {
	claims := auth.GetClaims(r.Context())
	if claims == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "Unauthorized",
			"message": "Authentication required",
		})
		return false
	}

	user, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil || user == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "Forbidden",
			"message": "Moderator privileges required",
		})
		return false
	}

	if !user.IsModerator && !user.IsAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "Forbidden",
			"message": "Moderator privileges required",
		})
		return false
	}

	return true
}
