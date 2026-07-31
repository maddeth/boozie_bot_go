package services

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PendingShoutout holds the data needed to retry a shoutout that failed (e.g.
// was rate limited). Login/DisplayName are stored so the retry doesn't need a
// fresh Helix lookup.
type PendingShoutout struct {
	UserID      string
	DisplayName string
	Login       string
}

// ShoutoutService manages auto-shoutout lists and per-stream session tracking.
// The Twitch API calls (sendShoutout) will be handled in internal/twitch/ - this
// service only manages the data layer and in-memory state.
type ShoutoutService struct {
	db *pgxpool.Pool

	mu                   sync.RWMutex
	autoShoutoutList     map[string]struct{}
	shoutedOutThisStream map[string]struct{}
	pending              map[string]PendingShoutout
}

// NewShoutoutService creates a new shoutout service.
func NewShoutoutService(db *pgxpool.Pool) *ShoutoutService {
	return &ShoutoutService{
		db:                   db,
		autoShoutoutList:     make(map[string]struct{}),
		shoutedOutThisStream: make(map[string]struct{}),
		pending:              make(map[string]PendingShoutout),
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
	s.pending = make(map[string]PendingShoutout)
	s.mu.Unlock()
	slog.Info("stream shoutout tracking reset")
}

// QueuePendingShoutout adds a shoutout to the retry queue (deduplicated by user
// ID). No-op if the user was already successfully shouted out this stream.
func (s *ShoutoutService) QueuePendingShoutout(p PendingShoutout) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, done := s.shoutedOutThisStream[p.UserID]; done {
		return
	}
	s.pending[p.UserID] = p
}

// GetPendingShoutouts returns a snapshot of the current retry queue.
func (s *ShoutoutService) GetPendingShoutouts() []PendingShoutout {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PendingShoutout, 0, len(s.pending))
	for _, p := range s.pending {
		out = append(out, p)
	}
	return out
}

// RemovePendingShoutout drops a shoutout from the retry queue (e.g. after it
// succeeds or is no longer worth retrying).
func (s *ShoutoutService) RemovePendingShoutout(userID string) {
	s.mu.Lock()
	delete(s.pending, userID)
	s.mu.Unlock()
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
