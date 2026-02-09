package handlers

import (
	"log/slog"
	"net/http"
	"time"
)

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// RequestLogger logs every HTTP request with method, path, status, and duration.
// Matches the JS requestLogger middleware.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)

		slog.Info("HTTP request",
			"method", r.Method,
			"url", r.URL.String(),
			"status", rw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"user_agent", r.UserAgent(),
			"ip", r.RemoteAddr,
		)
	})
}
