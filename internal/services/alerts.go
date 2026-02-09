package services

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Alert represents a row in the alerts table.
type Alert struct {
	ID         int    `json:"id"`
	EventTitle string `json:"event_title"`
	AudioURL   string `json:"audio_url"`
	GifURL     string `json:"gif_url"`
	DurationMS int    `json:"duration_ms"`
	Enabled    bool   `json:"enabled"`
}

// AlertConfig is the cached alert configuration keyed by event title.
type AlertConfig struct {
	Audio    string `json:"audio"`
	GifURL   string `json:"gifUrl"`
	Duration int    `json:"duration"`
}

// AlertService provides alert CRUD with an in-memory cache.
type AlertService struct {
	db *pgxpool.Pool

	mu             sync.RWMutex
	cache          map[string]AlertConfig
	cacheTimestamp  time.Time
	cacheTTL       time.Duration
}

// NewAlertService creates a new alert service with a 60-second cache.
func NewAlertService(db *pgxpool.Pool) *AlertService {
	return &AlertService{
		db:       db,
		cacheTTL: 60 * time.Second,
	}
}

// GetAlert looks up a single enabled alert by event title (no cache).
func (s *AlertService) GetAlert(ctx context.Context, eventTitle string) (*AlertConfig, error) {
	var a AlertConfig
	err := s.db.QueryRow(ctx,
		`SELECT audio_url, gif_url, duration_ms FROM alerts WHERE event_title = $1 AND enabled = true`,
		eventTitle,
	).Scan(&a.Audio, &a.GifURL, &a.Duration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAllAlerts returns all enabled alerts as a map keyed by event title. Uses cache.
func (s *AlertService) GetAllAlerts(ctx context.Context) (map[string]AlertConfig, error) {
	s.mu.RLock()
	if s.cache != nil && time.Since(s.cacheTimestamp) < s.cacheTTL {
		result := s.cache
		s.mu.RUnlock()
		return result, nil
	}
	s.mu.RUnlock()

	rows, err := s.db.Query(ctx,
		`SELECT event_title, audio_url, gif_url, duration_ms FROM alerts WHERE enabled = true ORDER BY event_title`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	alerts := make(map[string]AlertConfig)
	for rows.Next() {
		var title string
		var a AlertConfig
		if err := rows.Scan(&title, &a.Audio, &a.GifURL, &a.Duration); err != nil {
			return nil, err
		}
		alerts[title] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cache = alerts
	s.cacheTimestamp = time.Now()
	s.mu.Unlock()

	return alerts, nil
}

// UpdateAlert partially updates an alert by event title. Clears cache.
func (s *AlertService) UpdateAlert(ctx context.Context, eventTitle string, audioURL, gifURL *string, durationMS *int, enabled *bool) (*Alert, error) {
	var a Alert
	err := s.db.QueryRow(ctx,
		`UPDATE alerts SET
			audio_url = COALESCE($2, audio_url),
			gif_url = COALESCE($3, gif_url),
			duration_ms = COALESCE($4, duration_ms),
			enabled = COALESCE($5, enabled),
			updated_at = CURRENT_TIMESTAMP
		 WHERE event_title = $1 RETURNING id, event_title, audio_url, gif_url, duration_ms, enabled`,
		eventTitle, audioURL, gifURL, durationMS, enabled,
	).Scan(&a.ID, &a.EventTitle, &a.AudioURL, &a.GifURL, &a.DurationMS, &a.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	s.clearCache()
	return &a, nil
}

// CreateAlert inserts a new alert. Clears cache.
func (s *AlertService) CreateAlert(ctx context.Context, eventTitle, audioURL, gifURL string, durationMS int) (*Alert, error) {
	if durationMS == 0 {
		durationMS = 5000
	}

	var a Alert
	err := s.db.QueryRow(ctx,
		`INSERT INTO alerts (event_title, audio_url, gif_url, duration_ms) VALUES ($1, $2, $3, $4)
		 RETURNING id, event_title, audio_url, gif_url, duration_ms, enabled`,
		eventTitle, audioURL, gifURL, durationMS,
	).Scan(&a.ID, &a.EventTitle, &a.AudioURL, &a.GifURL, &a.DurationMS, &a.Enabled)
	if err != nil {
		slog.Error("failed to create alert", "error", err, "event_title", eventTitle)
		return nil, err
	}

	s.clearCache()
	return &a, nil
}

// DeleteAlert hard-deletes an alert. Clears cache.
func (s *AlertService) DeleteAlert(ctx context.Context, eventTitle string) (*Alert, error) {
	var a Alert
	err := s.db.QueryRow(ctx,
		`DELETE FROM alerts WHERE event_title = $1 RETURNING id, event_title, audio_url, gif_url, duration_ms, enabled`,
		eventTitle,
	).Scan(&a.ID, &a.EventTitle, &a.AudioURL, &a.GifURL, &a.DurationMS, &a.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	s.clearCache()
	return &a, nil
}

func (s *AlertService) clearCache() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}
