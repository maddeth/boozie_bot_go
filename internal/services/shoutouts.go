package services

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ShoutoutService manages auto-shoutout lists and per-stream session tracking.
// The Twitch API calls (sendShoutout) will be handled in internal/twitch/ — this
// service only manages the data layer and in-memory state.
type ShoutoutService struct {
	db *pgxpool.Pool

	mu                   sync.RWMutex
	autoShoutoutList     map[string]struct{}
	shoutedOutThisStream map[string]struct{}
}

// NewShoutoutService creates a new shoutout service.
func NewShoutoutService(db *pgxpool.Pool) *ShoutoutService {
	return &ShoutoutService{
		db:                   db,
		autoShoutoutList:     make(map[string]struct{}),
		shoutedOutThisStream: make(map[string]struct{}),
	}
}

// LoadAutoShoutoutList loads the auto-shoutout list from the database.
func (s *ShoutoutService) LoadAutoShoutoutList(ctx context.Context) error {
	rows, err := s.db.Query(ctx, `SELECT user_id FROM auto_shoutouts`)
	if err != nil {
		return err
	}
	defer rows.Close()

	list := make(map[string]struct{})
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return err
		}
		list[userID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.autoShoutoutList = list
	s.mu.Unlock()

	slog.Info("auto-shoutout list loaded", "count", len(list))
	return nil
}

// ShouldAutoShoutout checks if a user should receive an auto-shoutout.
func (s *ShoutoutService) ShouldAutoShoutout(userID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, inList := s.autoShoutoutList[userID]
	_, alreadyShouted := s.shoutedOutThisStream[userID]
	return inList && !alreadyShouted
}

// MarkShoutedOut records that a user has been shouted out this stream.
func (s *ShoutoutService) MarkShoutedOut(userID string) {
	s.mu.Lock()
	s.shoutedOutThisStream[userID] = struct{}{}
	s.mu.Unlock()
}

// AddToAutoShoutoutList adds a user to the auto-shoutout list (in-memory only).
func (s *ShoutoutService) AddToAutoShoutoutList(userID string) {
	s.mu.Lock()
	s.autoShoutoutList[userID] = struct{}{}
	s.mu.Unlock()
}

// RemoveFromAutoShoutoutList removes a user from the auto-shoutout list (in-memory only).
func (s *ShoutoutService) RemoveFromAutoShoutoutList(userID string) {
	s.mu.Lock()
	delete(s.autoShoutoutList, userID)
	s.mu.Unlock()
}

// ResetStreamShoutouts clears the per-stream shoutout tracking. Call when stream ends.
func (s *ShoutoutService) ResetStreamShoutouts() {
	s.mu.Lock()
	s.shoutedOutThisStream = make(map[string]struct{})
	s.mu.Unlock()
	slog.Info("stream shoutout tracking reset")
}

// GetShoutedOutUsers returns the set of users shouted out this stream.
func (s *ShoutoutService) GetShoutedOutUsers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]string, 0, len(s.shoutedOutThisStream))
	for id := range s.shoutedOutThisStream {
		users = append(users, id)
	}
	return users
}

// GetAutoShoutoutList returns the current auto-shoutout list.
func (s *ShoutoutService) GetAutoShoutoutList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]string, 0, len(s.autoShoutoutList))
	for id := range s.autoShoutoutList {
		users = append(users, id)
	}
	return users
}
