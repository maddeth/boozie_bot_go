package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// contextKey is an unexported type for context keys in this package.
type contextKey int

const (
	claimsKey contextKey = iota
	userKey
)

// User represents the database user record needed for role checks.
type User struct {
	ID              int    `json:"id"`
	TwitchUserID    string `json:"twitch_user_id"`
	Username        string `json:"username"`
	SupabaseUserID  string `json:"supabase_user_id"`
	IsModerator     bool   `json:"is_moderator"`
	IsAdmin         bool   `json:"is_admin"`
}

// Middleware provides HTTP middleware for authentication and authorization.
type Middleware struct {
	verifier *JWTVerifier
	db       *pgxpool.Pool
}

// NewMiddleware creates auth middleware with the given JWT verifier and DB pool.
func NewMiddleware(verifier *JWTVerifier, db *pgxpool.Pool) *Middleware {
	return &Middleware{verifier: verifier, db: db}
}

// AuthenticateToken verifies the JWT from the Authorization header and stores
// the claims in the request context. Matches the JS authenticateToken middleware.
func (m *Middleware) AuthenticateToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			slog.Warn("missing or invalid Authorization header",
				"ip", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":   "Unauthorized",
				"message": "Missing or invalid Authorization header",
			})
			return
		}

		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		claims, err := m.verifier.Verify(tokenString)
		if err != nil {
			slog.Warn("invalid JWT token",
				"ip", r.RemoteAddr,
				"user_agent", r.UserAgent(),
				"error", err,
			)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":   "Unauthorized",
				"message": "Invalid token",
			})
			return
		}

		slog.Debug("user authenticated",
			"user_id", claims.Subject,
			"email", claims.Email,
		)

		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireModeratorRole checks that the authenticated user has moderator or admin privileges.
// Must be used after AuthenticateToken.
func (m *Middleware) RequireModeratorRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := GetClaims(r.Context())
		if claims == nil || claims.Subject == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":   "Unauthorized",
				"message": "Authentication required",
			})
			return
		}

		user, err := m.getUserBySupabaseID(r.Context(), claims.Subject)
		if err != nil {
			slog.Error("error checking moderator role",
				"error", err,
				"user_id", claims.Subject,
			)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error":   "Internal server error",
				"message": "Failed to verify permissions",
			})
			return
		}

		if user == nil {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "Forbidden",
				"message": "User not found",
			})
			return
		}

		if !user.IsModerator && !user.IsAdmin {
			slog.Warn("non-moderator attempted to access moderator endpoint",
				"twitch_user_id", user.TwitchUserID,
				"username", user.Username,
				"endpoint", r.URL.Path,
			)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "Forbidden",
				"message": "Moderator privileges required",
			})
			return
		}

		ctx := context.WithValue(r.Context(), userKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAdminRole checks that the authenticated user has admin privileges.
// Must be used after AuthenticateToken.
func (m *Middleware) RequireAdminRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := GetClaims(r.Context())
		if claims == nil || claims.Subject == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":   "Unauthorized",
				"message": "Authentication required",
			})
			return
		}

		user, err := m.getUserBySupabaseID(r.Context(), claims.Subject)
		if err != nil {
			slog.Error("error checking admin role",
				"error", err,
				"user_id", claims.Subject,
			)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error":   "Internal server error",
				"message": "Failed to verify permissions",
			})
			return
		}

		if user == nil {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "Forbidden",
				"message": "User not found",
			})
			return
		}

		if !user.IsAdmin {
			slog.Warn("non-admin attempted to access admin endpoint",
				"twitch_user_id", user.TwitchUserID,
				"username", user.Username,
				"endpoint", r.URL.Path,
			)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "Forbidden",
				"message": "Admin privileges required",
			})
			return
		}

		ctx := context.WithValue(r.Context(), userKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// getUserBySupabaseID looks up a user by their Supabase UUID.
// Matches the JS getUserBySupabaseId function.
func (m *Middleware) getUserBySupabaseID(ctx context.Context, supabaseUserID string) (*User, error) {
	var user User
	err := m.db.QueryRow(ctx,
		`SELECT id, twitch_user_id, username, supabase_user_id, is_moderator, is_admin
		 FROM users WHERE supabase_user_id = $1`,
		supabaseUserID,
	).Scan(&user.ID, &user.TwitchUserID, &user.Username, &user.SupabaseUserID, &user.IsModerator, &user.IsAdmin)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// GetClaims retrieves the JWT claims from the request context.
func GetClaims(ctx context.Context) *Claims {
	claims, _ := ctx.Value(claimsKey).(*Claims)
	return claims
}

// GetUser retrieves the database user from the request context.
// Only available after RequireModeratorRole or RequireAdminRole.
func GetUser(ctx context.Context) *User {
	user, _ := ctx.Value(userKey).(*User)
	return user
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
