package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
)

// CommandHandler handles /api/commands/* endpoints.
type CommandHandler struct {
	commands *services.CommandService
	users    *services.UserService
	db       *pgxpool.Pool
	auth     *auth.Middleware
}

// NewCommandHandler creates a new command handler.
func NewCommandHandler(commands *services.CommandService, users *services.UserService, db *pgxpool.Pool, authMW *auth.Middleware) *CommandHandler {
	return &CommandHandler{commands: commands, users: users, db: db, auth: authMW}
}

// Register registers command routes on the given mux.
func (h *CommandHandler) Register(mux *http.ServeMux) {
	// Public
	mux.HandleFunc("GET /api/commands/", h.getEnabled)
	mux.HandleFunc("GET /api/commands/trigger/{trigger}", h.getByTrigger)

	// Auth-protected
	mux.Handle("GET /api/commands/all", h.auth.AuthenticateToken(http.HandlerFunc(h.getAll)))
	mux.Handle("POST /api/commands/", h.auth.AuthenticateToken(http.HandlerFunc(h.create)))
	mux.Handle("PUT /api/commands/{id}", h.auth.AuthenticateToken(http.HandlerFunc(h.update)))
	mux.Handle("DELETE /api/commands/{id}", h.auth.AuthenticateToken(http.HandlerFunc(h.deleteCmd)))

	// Internal
	mux.HandleFunc("POST /api/commands/{id}/usage", h.incrementUsage)
}

// dbCommand is used for serializing full command rows from the database.
type dbCommand struct {
	ID          int     `json:"id"`
	Trigger     string  `json:"trigger"`
	Response    *string `json:"response"`
	Cooldown    int     `json:"cooldown"`
	Permission  string  `json:"permission"`
	Enabled     bool    `json:"enabled"`
	UsageCount  int     `json:"usage_count"`
	CreatedBy   *string `json:"created_by"`
	CreatedAt   any     `json:"created_at"`
	UpdatedAt   any     `json:"updated_at"`
	LastUsedAt  any     `json:"last_used_at"`
	TriggerType string  `json:"trigger_type"`
	AudioURL    *string `json:"audio_url"`
	EggCost     int     `json:"egg_cost"`
}

func (h *CommandHandler) getEnabled(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(),
		`SELECT id, trigger, response, cooldown, permission, enabled, usage_count, created_by, created_at,
		 COALESCE(trigger_type, 'exact') as trigger_type, audio_url, COALESCE(egg_cost, 0) as egg_cost
		 FROM custom_commands WHERE enabled = true ORDER BY trigger ASC`,
	)
	if err != nil {
		logAndError(w, "Failed to retrieve commands", err)
		return
	}
	defer rows.Close()

	var commands []dbCommand
	for rows.Next() {
		var c dbCommand
		if err := rows.Scan(&c.ID, &c.Trigger, &c.Response, &c.Cooldown, &c.Permission, &c.Enabled,
			&c.UsageCount, &c.CreatedBy, &c.CreatedAt, &c.TriggerType, &c.AudioURL, &c.EggCost); err != nil {
			logAndError(w, "Failed to retrieve commands", err)
			return
		}
		commands = append(commands, c)
	}
	if commands == nil {
		commands = []dbCommand{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"commands": commands,
		"count":    len(commands),
	})
}

func (h *CommandHandler) getAll(w http.ResponseWriter, r *http.Request) {
	// Require moderator
	if err := h.requireModerator(r.Context(), r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Forbidden", "message": "Moderator privileges required"})
		return
	}

	rows, err := h.db.Query(r.Context(),
		`SELECT id, trigger, response, cooldown, permission, enabled, usage_count, created_by, created_at, updated_at, last_used_at,
		 COALESCE(trigger_type, 'exact') as trigger_type, audio_url, COALESCE(egg_cost, 0) as egg_cost
		 FROM custom_commands ORDER BY trigger ASC`,
	)
	if err != nil {
		logAndError(w, "Failed to retrieve commands", err)
		return
	}
	defer rows.Close()

	var commands []dbCommand
	for rows.Next() {
		var c dbCommand
		if err := rows.Scan(&c.ID, &c.Trigger, &c.Response, &c.Cooldown, &c.Permission, &c.Enabled,
			&c.UsageCount, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.LastUsedAt,
			&c.TriggerType, &c.AudioURL, &c.EggCost); err != nil {
			logAndError(w, "Failed to retrieve commands", err)
			return
		}
		commands = append(commands, c)
	}
	if commands == nil {
		commands = []dbCommand{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"commands": commands,
		"count":    len(commands),
	})
}

func (h *CommandHandler) getByTrigger(w http.ResponseWriter, r *http.Request) {
	trigger := r.PathValue("trigger")

	var c dbCommand
	err := h.db.QueryRow(r.Context(),
		`SELECT id, trigger, response, cooldown, permission, enabled, usage_count, created_by, created_at,
		 COALESCE(trigger_type, 'exact'), audio_url, COALESCE(egg_cost, 0)
		 FROM custom_commands WHERE trigger = $1 AND enabled = true`, trigger,
	).Scan(&c.ID, &c.Trigger, &c.Response, &c.Cooldown, &c.Permission, &c.Enabled,
		&c.UsageCount, &c.CreatedBy, &c.CreatedAt, &c.TriggerType, &c.AudioURL, &c.EggCost)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found", "message": "Command not found"})
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *CommandHandler) create(w http.ResponseWriter, r *http.Request) {
	// Require admin
	if err := h.requireAdmin(r.Context(), r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Forbidden", "message": "Bot admin privileges required"})
		return
	}

	user, _ := h.getRequestUser(r.Context(), r)

	var body struct {
		Trigger     string  `json:"trigger"`
		Response    *string `json:"response"`
		Cooldown    int     `json:"cooldown"`
		Permission  string  `json:"permission"`
		TriggerType string  `json:"trigger_type"`
		AudioURL    *string `json:"audio_url"`
		EggCost     int     `json:"egg_cost"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if body.Trigger == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "Trigger is required"})
		return
	}

	hasResponse := body.Response != nil && strings.TrimSpace(*body.Response) != ""
	hasAudio := body.AudioURL != nil && strings.TrimSpace(*body.AudioURL) != ""
	if !hasResponse && !hasAudio {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "Either response or audio_url is required"})
		return
	}

	if body.Permission == "" {
		body.Permission = "everyone"
	}
	if !isValidPermission(body.Permission) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "Invalid permission level"})
		return
	}

	if body.TriggerType == "" {
		body.TriggerType = "exact"
	}
	if !isValidTriggerType(body.TriggerType) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "Invalid trigger type"})
		return
	}

	if body.TriggerType == "regex" {
		if _, err := regexp.Compile(body.Trigger); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "Invalid regex pattern"})
			return
		}
	}

	// Check for existing command
	var existingID int
	err := h.db.QueryRow(r.Context(), `SELECT id FROM custom_commands WHERE trigger = $1`, body.Trigger).Scan(&existingID)
	if err == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Conflict", "message": "Command with this trigger already exists"})
		return
	}

	var createdBy *string
	if user != nil {
		createdBy = &user.Username
	}

	var c dbCommand
	err = h.db.QueryRow(r.Context(),
		`INSERT INTO custom_commands (trigger, response, cooldown, permission, created_by, trigger_type, audio_url, egg_cost)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, trigger, response, cooldown, permission, enabled, usage_count, created_by, created_at,
		 COALESCE(trigger_type, 'exact'), audio_url, COALESCE(egg_cost, 0)`,
		body.Trigger, body.Response, body.Cooldown, body.Permission, createdBy, body.TriggerType, body.AudioURL, body.EggCost,
	).Scan(&c.ID, &c.Trigger, &c.Response, &c.Cooldown, &c.Permission, &c.Enabled,
		&c.UsageCount, &c.CreatedBy, &c.CreatedAt, &c.TriggerType, &c.AudioURL, &c.EggCost)
	if err != nil {
		logAndError(w, "Failed to create command", err)
		return
	}

	slog.Info("command created", "trigger", body.Trigger, "created_by", createdBy)

	// Reload command cache
	h.commands.ReloadCommands(r.Context())

	writeJSON(w, http.StatusCreated, c)
}

func (h *CommandHandler) update(w http.ResponseWriter, r *http.Request) {
	// Require admin
	if err := h.requireAdmin(r.Context(), r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Forbidden", "message": "Bot admin privileges required"})
		return
	}

	id, ok := parsePathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid command ID")
		return
	}

	// Check command exists
	var existingID int
	if err := h.db.QueryRow(r.Context(), `SELECT id FROM custom_commands WHERE id = $1`, id).Scan(&existingID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found", "message": "Command not found"})
		return
	}

	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Build dynamic update query
	updates := []string{}
	values := []any{}
	idx := 1

	if v, ok := body["trigger"]; ok {
		trigger := fmt.Sprintf("%v", v)
		// Check for conflict
		var conflictID int
		if err := h.db.QueryRow(r.Context(), `SELECT id FROM custom_commands WHERE trigger = $1 AND id != $2`, trigger, id).Scan(&conflictID); err == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Conflict", "message": "Another command with this trigger already exists"})
			return
		}
		updates = append(updates, fmt.Sprintf("trigger = $%d", idx))
		values = append(values, trigger)
		idx++
	}

	for _, field := range []string{"response", "audio_url"} {
		if v, ok := body[field]; ok {
			updates = append(updates, fmt.Sprintf("%s = $%d", field, idx))
			values = append(values, v)
			idx++
		}
	}

	for _, field := range []string{"cooldown", "egg_cost"} {
		if v, ok := body[field]; ok {
			updates = append(updates, fmt.Sprintf("%s = $%d", field, idx))
			n, _ := v.(float64) // JSON numbers are float64
			values = append(values, int(n))
			idx++
		}
	}

	if v, ok := body["permission"]; ok {
		perm := fmt.Sprintf("%v", v)
		if !isValidPermission(perm) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "Invalid permission level"})
			return
		}
		updates = append(updates, fmt.Sprintf("permission = $%d", idx))
		values = append(values, perm)
		idx++
	}

	if v, ok := body["enabled"]; ok {
		updates = append(updates, fmt.Sprintf("enabled = $%d", idx))
		values = append(values, v)
		idx++
	}

	if v, ok := body["trigger_type"]; ok {
		tt := fmt.Sprintf("%v", v)
		if !isValidTriggerType(tt) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "Invalid trigger type"})
			return
		}
		if tt == "regex" {
			if t, ok := body["trigger"]; ok {
				if _, err := regexp.Compile(fmt.Sprintf("%v", t)); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "Invalid regex pattern"})
					return
				}
			}
		}
		updates = append(updates, fmt.Sprintf("trigger_type = $%d", idx))
		values = append(values, tt)
		idx++
	}

	if len(updates) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bad request", "message": "No valid fields to update"})
		return
	}

	values = append(values, id)
	query := fmt.Sprintf(
		`UPDATE custom_commands SET %s, updated_at = CURRENT_TIMESTAMP WHERE id = $%d
		 RETURNING id, trigger, response, cooldown, permission, enabled, usage_count, created_by, created_at,
		 COALESCE(trigger_type, 'exact'), audio_url, COALESCE(egg_cost, 0)`,
		strings.Join(updates, ", "), idx,
	)

	var c dbCommand
	err := h.db.QueryRow(r.Context(), query, values...).Scan(
		&c.ID, &c.Trigger, &c.Response, &c.Cooldown, &c.Permission, &c.Enabled,
		&c.UsageCount, &c.CreatedBy, &c.CreatedAt, &c.TriggerType, &c.AudioURL, &c.EggCost)
	if err != nil {
		logAndError(w, "Failed to update command", err)
		return
	}

	slog.Info("command updated", "id", id)
	h.commands.ReloadCommands(r.Context())
	writeJSON(w, http.StatusOK, c)
}

func (h *CommandHandler) deleteCmd(w http.ResponseWriter, r *http.Request) {
	// Require admin
	if err := h.requireAdmin(r.Context(), r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Forbidden", "message": "Bot admin privileges required"})
		return
	}

	id, ok := parsePathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid command ID")
		return
	}

	// Check exists
	var trigger string
	if err := h.db.QueryRow(r.Context(), `SELECT trigger FROM custom_commands WHERE id = $1`, id).Scan(&trigger); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found", "message": "Command not found"})
		return
	}

	if _, err := h.db.Exec(r.Context(), `DELETE FROM custom_commands WHERE id = $1`, id); err != nil {
		logAndError(w, "Failed to delete command", err)
		return
	}

	slog.Info("command deleted", "id", id, "trigger", trigger)
	h.commands.ReloadCommands(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (h *CommandHandler) incrementUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid command ID")
		return
	}

	if _, err := h.db.Exec(r.Context(),
		`UPDATE custom_commands SET usage_count = usage_count + 1, last_used_at = CURRENT_TIMESTAMP WHERE id = $1`, id,
	); err != nil {
		logAndError(w, "Failed to update usage count", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireModerator checks if the authenticated user is a moderator.
func (h *CommandHandler) requireModerator(ctx context.Context, r *http.Request) error {
	user, err := h.getRequestUser(ctx, r)
	if err != nil || user == nil {
		return fmt.Errorf("user not found")
	}
	if !user.IsModerator && !user.IsAdmin {
		return fmt.Errorf("not a moderator")
	}
	return nil
}

// requireAdmin checks if the authenticated user is an admin.
func (h *CommandHandler) requireAdmin(ctx context.Context, r *http.Request) error {
	user, err := h.getRequestUser(ctx, r)
	if err != nil || user == nil {
		return fmt.Errorf("user not found")
	}
	if !user.IsAdmin {
		return fmt.Errorf("not an admin")
	}
	return nil
}

// getRequestUser resolves the authenticated user from JWT claims.
func (h *CommandHandler) getRequestUser(ctx context.Context, r *http.Request) (*services.User, error) {
	claims := auth.GetClaims(r.Context())
	if claims == nil {
		return nil, fmt.Errorf("no claims")
	}
	return h.users.GetBySupabaseID(ctx, claims.Subject)
}

func isValidPermission(p string) bool {
	switch p {
	case "everyone", "subscriber", "vip", "moderator":
		return true
	}
	return false
}

func isValidTriggerType(t string) bool {
	switch t {
	case "exact", "contains", "regex":
		return true
	}
	return false
}
