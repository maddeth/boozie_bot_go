package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON decodes the request body into v.
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// parseIntParam parses an integer from a query parameter with a default and max value.
func parseIntParam(r *http.Request, key string, defaultVal, maxVal int) int {
	s := r.URL.Query().Get(key)
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return defaultVal
	}
	if maxVal > 0 && n > maxVal {
		return maxVal
	}
	return n
}

// pathParam extracts a path parameter from the URL using Go 1.22+ PathValue.
func pathParam(r *http.Request, name string) string {
	return r.PathValue(name)
}

// allowedOrigins is the set of origins permitted to make cross-origin requests.
var allowedOrigins = map[string]bool{
	"https://maddeth.com":                true,
	"https://www.maddeth.com":            true,
	"https://boozie-bot-web.pages.dev":   true,
	"https://boozie-bot.com":             true,
}

// setCORS sets CORS headers if the request origin is allowed.
func setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if allowedOrigins[origin] {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

// CORSMiddleware wraps a handler with CORS headers.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logAndError logs an error and writes a 500 response.
func logAndError(w http.ResponseWriter, msg string, err error) {
	slog.Error(msg, "error", err)
	writeError(w, http.StatusInternalServerError, msg)
}

// parsePathInt parses an integer from a URL path parameter.
func parsePathInt(r *http.Request, name string) (int, bool) {
	s := r.PathValue(name)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// sanitizeColourInput sanitises colour name input: lowercase, alphanumeric + spaces, max 60 chars.
func sanitizeColourInput(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || r == ' ' {
			b.WriteRune(r)
		}
	}
	result := b.String()
	if len(result) > 60 {
		result = result[:60]
	}
	return strings.TrimSpace(result)
}

// sanitizeHexInput sanitises hex colour input: uppercase, 6 hex chars.
func sanitizeHexInput(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "#")
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'F') {
			b.WriteRune(r)
		}
	}
	result := b.String()
	if len(result) >= 6 {
		return result[:6]
	}
	return result
}
