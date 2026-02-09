package handlers

import (
	"net/http"
	"strings"

	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
)

// UserMergeHandler handles /api/user-merge/* endpoints.
type UserMergeHandler struct {
	merge *services.UserMergeService
	auth  *auth.Middleware
}

// NewUserMergeHandler creates a new user merge handler.
func NewUserMergeHandler(merge *services.UserMergeService, authMW *auth.Middleware) *UserMergeHandler {
	return &UserMergeHandler{merge: merge, auth: authMW}
}

// Register registers user merge routes on the given mux.
func (h *UserMergeHandler) Register(mux *http.ServeMux) {
	// All admin-protected
	mux.Handle("POST /api/user-merge/preview", h.auth.AuthenticateToken(h.auth.RequireAdminRole(http.HandlerFunc(h.preview))))
	mux.Handle("POST /api/user-merge/execute", h.auth.AuthenticateToken(h.auth.RequireAdminRole(http.HandlerFunc(h.execute))))
	mux.Handle("GET /api/user-merge/history", h.auth.AuthenticateToken(h.auth.RequireAdminRole(http.HandlerFunc(h.allHistory))))
	mux.Handle("GET /api/user-merge/history/{userId}", h.auth.AuthenticateToken(h.auth.RequireAdminRole(http.HandlerFunc(h.userHistory))))
}

func (h *UserMergeHandler) preview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FromUserID string `json:"fromUserId"`
		ToUserID   string `json:"toUserId"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if body.FromUserID == "" || body.ToUserID == "" {
		writeError(w, http.StatusBadRequest, "Both fromUserId and toUserId are required")
		return
	}
	if body.FromUserID == body.ToUserID {
		writeError(w, http.StatusBadRequest, "Cannot merge user with themselves")
		return
	}

	preview, err := h.merge.PreviewMerge(r.Context(), body.FromUserID, body.ToUserID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		logAndError(w, "Failed to preview merge", err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (h *UserMergeHandler) execute(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	var body struct {
		FromUserID   string  `json:"fromUserId"`
		ToUserID     string  `json:"toUserId"`
		Reason       *string `json:"reason"`
		DeleteSource bool    `json:"deleteSource"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if body.FromUserID == "" || body.ToUserID == "" {
		writeError(w, http.StatusBadRequest, "Both fromUserId and toUserId are required")
		return
	}
	if body.FromUserID == body.ToUserID {
		writeError(w, http.StatusBadRequest, "Cannot merge user with themselves")
		return
	}

	// Default reason
	reason := body.Reason
	if reason == nil {
		defaultReason := "Account merge requested"
		reason = &defaultReason
	}

	result, err := h.merge.MergeUserEggs(r.Context(), body.FromUserID, body.ToUserID, user.TwitchUserID, user.Username, reason, body.DeleteSource)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		logAndError(w, "Failed to execute merge", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *UserMergeHandler) allHistory(w http.ResponseWriter, r *http.Request) {
	limit := parseIntParam(r, "limit", 50, 0)

	history, err := h.merge.GetAllMergeHistory(r.Context(), limit)
	if err != nil {
		logAndError(w, "Failed to fetch merge history", err)
		return
	}
	writeJSON(w, http.StatusOK, history)
}

func (h *UserMergeHandler) userHistory(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userId")
	limit := parseIntParam(r, "limit", 10, 0)

	history, err := h.merge.GetMergeHistory(r.Context(), userID, limit)
	if err != nil {
		logAndError(w, "Failed to fetch user merge history", err)
		return
	}
	writeJSON(w, http.StatusOK, history)
}
