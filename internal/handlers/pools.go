package handlers

import (
	"net/http"
	"strings"

	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
)

// PoolHandler handles /api/pools/* endpoints.
type PoolHandler struct {
	pools *services.PoolService
	users *services.UserService
	auth  *auth.Middleware
}

// NewPoolHandler creates a new pool handler.
func NewPoolHandler(pools *services.PoolService, users *services.UserService, authMW *auth.Middleware) *PoolHandler {
	return &PoolHandler{pools: pools, users: users, auth: authMW}
}

// Register registers pool routes on the given mux.
func (h *PoolHandler) Register(mux *http.ServeMux) {
	// Public
	mux.HandleFunc("GET /api/pools/", h.getAll)
	mux.HandleFunc("GET /api/pools/{poolName}", h.getOne)
	mux.HandleFunc("GET /api/pools/{poolName}/donations", h.donations)

	// Auth-protected
	mux.Handle("POST /api/pools/", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.create))))
	mux.Handle("POST /api/pools/{poolName}/donate", h.auth.AuthenticateToken(http.HandlerFunc(h.donate)))
	mux.Handle("POST /api/pools/{poolName}/admin", h.auth.AuthenticateToken(h.auth.RequireAdminRole(http.HandlerFunc(h.adminAdjust))))
}

func (h *PoolHandler) getAll(w http.ResponseWriter, r *http.Request) {
	pools, err := h.pools.GetAllPools(r.Context())
	if err != nil {
		logAndError(w, "Failed to fetch pools", err)
		return
	}
	writeJSON(w, http.StatusOK, pools)
}

func (h *PoolHandler) getOne(w http.ResponseWriter, r *http.Request) {
	poolName := r.PathValue("poolName")
	pool, err := h.pools.GetPool(r.Context(), poolName)
	if err != nil {
		logAndError(w, "Failed to fetch pool", err)
		return
	}
	if pool == nil {
		writeError(w, http.StatusNotFound, "Pool not found")
		return
	}
	writeJSON(w, http.StatusOK, pool)
}

func (h *PoolHandler) donations(w http.ResponseWriter, r *http.Request) {
	poolName := r.PathValue("poolName")
	limit := parseIntParam(r, "limit", 10, 0)

	txns, err := h.pools.GetRecentDonations(r.Context(), poolName, limit)
	if err != nil {
		logAndError(w, "Failed to fetch donations", err)
		return
	}
	writeJSON(w, http.StatusOK, txns)
}

func (h *PoolHandler) create(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	var body struct {
		PoolName    string `json:"poolName"`
		Description string `json:"description"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if body.PoolName == "" {
		writeError(w, http.StatusBadRequest, "Pool name is required")
		return
	}

	pool, err := h.pools.CreatePool(r.Context(), body.PoolName, body.Description, user.TwitchUserID, user.Username)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		logAndError(w, "Failed to create pool", err)
		return
	}
	writeJSON(w, http.StatusCreated, pool)
}

func (h *PoolHandler) donate(w http.ResponseWriter, r *http.Request) {
	poolName := r.PathValue("poolName")

	claims := auth.GetClaims(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	// Resolve user's Twitch info
	user, err := h.users.GetBySupabaseID(r.Context(), claims.Subject)
	if err != nil {
		logAndError(w, "Failed to verify user", err)
		return
	}
	if user == nil || user.TwitchUserID == nil {
		writeError(w, http.StatusBadRequest, "No Twitch account linked")
		return
	}

	var body struct {
		Amount int `json:"amount"`
	}
	if err := readJSON(r, &body); err != nil || body.Amount < 1 {
		writeError(w, http.StatusBadRequest, "Invalid donation amount")
		return
	}

	pool, err := h.pools.DonateToPool(r.Context(), poolName, *user.TwitchUserID, user.Username, body.Amount)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, msg)
		} else if strings.Contains(msg, "insufficient") || strings.Contains(msg, "not active") {
			writeError(w, http.StatusBadRequest, msg)
		} else {
			logAndError(w, "Failed to process donation", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, pool)
}

func (h *PoolHandler) adminAdjust(w http.ResponseWriter, r *http.Request) {
	poolName := r.PathValue("poolName")

	user := auth.GetUser(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	var body struct {
		Amount int     `json:"amount"`
		Notes  *string `json:"notes"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if body.Amount == 0 {
		writeError(w, http.StatusBadRequest, "Invalid adjustment amount")
		return
	}

	pool, err := h.pools.AdminAdjustPool(r.Context(), poolName, body.Amount, user.TwitchUserID, user.Username, body.Notes)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") || strings.Contains(msg, "inactive") {
			writeError(w, http.StatusNotFound, msg)
		} else if strings.Contains(msg, "negative") {
			writeError(w, http.StatusBadRequest, msg)
		} else {
			logAndError(w, "Failed to adjust pool", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, pool)
}
