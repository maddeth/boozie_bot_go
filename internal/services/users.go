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

// User represents a row in the users table.
type User struct {
	ID                 int        `json:"id"`
	TwitchUserID       *string    `json:"twitch_user_id"`
	Username           string     `json:"username"`
	DisplayName        *string    `json:"display_name"`
	SupabaseUserID     *string    `json:"supabase_user_id"`
	Email              *string    `json:"email"`
	IsModerator        bool       `json:"is_moderator"`
	IsAdmin            bool       `json:"is_admin"`
	IsSubscriber       bool       `json:"is_subscriber"`
	SubscriptionTier   *string    `json:"subscription_tier"`
	SubscriptionUpdated *time.Time `json:"subscription_updated"`
	ModeratorSince     *time.Time `json:"moderator_since"`
	ModeratorUpdated   *time.Time `json:"moderator_updated"`
	LastSeen           *time.Time `json:"last_seen"`
	CreatedAt          time.Time  `json:"created_at"`
}

// UserStats contains aggregate user statistics.
type UserStats struct {
	TotalUsers      int `json:"totalUsers"`
	Moderators      int `json:"moderators"`
	Subscribers     int `json:"subscribers"`
	RegisteredUsers int `json:"registeredUsers"`
	ActiveWeekly    int `json:"activeWeekly"`
}

type userCacheEntry struct {
	user      *User
	timestamp time.Time
}

// UserService provides user CRUD operations with caching.
type UserService struct {
	db       *pgxpool.Pool
	mu       sync.RWMutex
	cache    map[string]userCacheEntry
	cacheTTL time.Duration
}

// NewUserService creates a new user service with a 5-minute cache.
func NewUserService(db *pgxpool.Pool) *UserService {
	return &UserService{
		db:       db,
		cache:    make(map[string]userCacheEntry),
		cacheTTL: 5 * time.Minute,
	}
}

// userScanFields returns the scan destinations for a full user row.
func userScanFields(u *User) []any {
	return []any{
		&u.ID, &u.TwitchUserID, &u.Username, &u.DisplayName,
		&u.SupabaseUserID, &u.Email, &u.IsModerator, &u.IsAdmin,
		&u.IsSubscriber, &u.SubscriptionTier, &u.SubscriptionUpdated,
		&u.ModeratorSince, &u.ModeratorUpdated, &u.LastSeen, &u.CreatedAt,
	}
}

const userSelectCols = `id, twitch_user_id, username, display_name, supabase_user_id, email,
	is_moderator, is_admin, is_subscriber, subscription_tier, subscription_updated,
	moderator_since, moderator_updated, last_seen, created_at`

// GetOrCreateUser looks up or creates a user by Twitch ID, with caching.
// Matches the 3-step resolution in JS: lookup by twitch ID -> username -> create.
func (s *UserService) GetOrCreateUser(ctx context.Context, twitchUserID, username string, displayName *string) (*User, error) {
	cacheKey := "twitch:" + twitchUserID

	// Check cache
	s.mu.RLock()
	entry, ok := s.cache[cacheKey]
	s.mu.RUnlock()
	if ok && time.Since(entry.timestamp) < s.cacheTTL {
		// Update last_seen if stale (>60s)
		if entry.user.LastSeen == nil || time.Since(*entry.user.LastSeen) > 60*time.Second {
			go s.touchLastSeen(context.Background(), twitchUserID, username, displayName)
		}
		return entry.user, nil
	}

	// Step 1: Lookup by Twitch ID
	var u User
	err := s.db.QueryRow(ctx,
		`SELECT `+userSelectCols+` FROM users WHERE twitch_user_id = $1`, twitchUserID,
	).Scan(userScanFields(&u)...)

	if err == nil {
		// Found — update username/display_name/last_seen if needed
		needsUpdate := u.Username != username || (displayName != nil && (u.DisplayName == nil || *u.DisplayName != *displayName))
		if needsUpdate || u.LastSeen == nil || time.Since(*u.LastSeen) > 60*time.Second {
			s.db.Exec(ctx,
				`UPDATE users SET username = $2, display_name = COALESCE($3, display_name), last_seen = NOW() WHERE twitch_user_id = $1`,
				twitchUserID, username, displayName,
			)
			u.Username = username
			if displayName != nil {
				u.DisplayName = displayName
			}
			now := time.Now()
			u.LastSeen = &now
		}
		s.cacheUser(cacheKey, &u)
		return &u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Step 2: Fallback to case-insensitive username lookup
	err = s.db.QueryRow(ctx,
		`SELECT `+userSelectCols+` FROM users WHERE LOWER(username) = LOWER($1)`, username,
	).Scan(userScanFields(&u)...)

	if err == nil {
		// Found by username — backfill twitch_user_id
		s.db.Exec(ctx,
			`UPDATE users SET twitch_user_id = $1, display_name = COALESCE($2, display_name), username = $3, last_seen = NOW() WHERE id = $4`,
			twitchUserID, displayName, username, u.ID,
		)
		u.TwitchUserID = &twitchUserID
		u.Username = username
		if displayName != nil {
			u.DisplayName = displayName
		}
		now := time.Now()
		u.LastSeen = &now
		s.cacheUser(cacheKey, &u)
		return &u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Step 3: Create new user
	err = s.db.QueryRow(ctx,
		`INSERT INTO users (twitch_user_id, username, display_name, last_seen) VALUES ($1, $2, $3, NOW())
		 RETURNING `+userSelectCols,
		twitchUserID, username, displayName,
	).Scan(userScanFields(&u)...)
	if err != nil {
		return nil, err
	}

	slog.Info("new user created", "twitch_user_id", twitchUserID, "username", username)
	s.cacheUser(cacheKey, &u)
	return &u, nil
}

// touchLastSeen fires a background update for last_seen/username/display_name.
func (s *UserService) touchLastSeen(ctx context.Context, twitchUserID, username string, displayName *string) {
	_, err := s.db.Exec(ctx,
		`UPDATE users SET username = $2, display_name = COALESCE($3, display_name), last_seen = NOW() WHERE twitch_user_id = $1`,
		twitchUserID, username, displayName,
	)
	if err != nil {
		slog.Error("failed to update last_seen", "error", err, "twitch_user_id", twitchUserID)
	}
}

// UpdateModeratorStatus updates a user's moderator status.
func (s *UserService) UpdateModeratorStatus(ctx context.Context, twitchUserID string, isModerator bool) error {
	s.invalidateCache("twitch:" + twitchUserID)

	_, err := s.db.Exec(ctx,
		`UPDATE users SET
			is_moderator = $2,
			moderator_updated = NOW(),
			moderator_since = CASE
				WHEN $2 = true AND moderator_since IS NULL THEN NOW()
				ELSE moderator_since
			END
		 WHERE twitch_user_id = $1`,
		twitchUserID, isModerator,
	)
	return err
}

// UpdateSubscriptionStatus updates a user's subscription status.
func (s *UserService) UpdateSubscriptionStatus(ctx context.Context, twitchUserID string, isSubscriber bool, tier *string) error {
	s.invalidateCache("twitch:" + twitchUserID)

	_, err := s.db.Exec(ctx,
		`UPDATE users SET is_subscriber = $2, subscription_tier = $3, subscription_updated = NOW()
		 WHERE twitch_user_id = $1`,
		twitchUserID, isSubscriber, tier,
	)
	return err
}

// LinkSupabaseUser links a Supabase auth ID to a Twitch user.
func (s *UserService) LinkSupabaseUser(ctx context.Context, twitchUserID, supabaseUserID string, email *string) error {
	s.invalidateCache("twitch:" + twitchUserID)

	_, err := s.db.Exec(ctx,
		`UPDATE users SET supabase_user_id = $2, email = $3 WHERE twitch_user_id = $1`,
		twitchUserID, supabaseUserID, email,
	)
	return err
}

// GetByTwitchID looks up a user by Twitch user ID.
func (s *UserService) GetByTwitchID(ctx context.Context, twitchUserID string) (*User, error) {
	var u User
	err := s.db.QueryRow(ctx,
		`SELECT `+userSelectCols+` FROM users WHERE twitch_user_id = $1`, twitchUserID,
	).Scan(userScanFields(&u)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetBySupabaseID looks up a user by Supabase user ID.
func (s *UserService) GetBySupabaseID(ctx context.Context, supabaseUserID string) (*User, error) {
	var u User
	err := s.db.QueryRow(ctx,
		`SELECT `+userSelectCols+` FROM users WHERE supabase_user_id = $1`, supabaseUserID,
	).Scan(userScanFields(&u)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetByUsername looks up a user by username (case-insensitive).
func (s *UserService) GetByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := s.db.QueryRow(ctx,
		`SELECT `+userSelectCols+` FROM users WHERE LOWER(username) = LOWER($1)`, username,
	).Scan(userScanFields(&u)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetModerators returns all moderators ordered by when they became a mod.
func (s *UserService) GetModerators(ctx context.Context) ([]User, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+userSelectCols+` FROM users WHERE is_moderator = true ORDER BY moderator_since ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(userScanFields(&u)...); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// IsModerator checks if a user is a moderator.
func (s *UserService) IsModerator(ctx context.Context, twitchUserID string) (bool, error) {
	var isMod bool
	err := s.db.QueryRow(ctx,
		`SELECT is_moderator FROM users WHERE twitch_user_id = $1`, twitchUserID,
	).Scan(&isMod)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return isMod, err
}

// GetUserStats returns aggregate statistics about users.
func (s *UserService) GetUserStats(ctx context.Context) (*UserStats, error) {
	var stats UserStats
	err := s.db.QueryRow(ctx,
		`SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE is_moderator = true),
			COUNT(*) FILTER (WHERE is_subscriber = true),
			COUNT(*) FILTER (WHERE supabase_user_id IS NOT NULL),
			COUNT(*) FILTER (WHERE last_seen > NOW() - INTERVAL '7 days')
		 FROM users`,
	).Scan(&stats.TotalUsers, &stats.Moderators, &stats.Subscribers, &stats.RegisteredUsers, &stats.ActiveWeekly)
	if err != nil {
		return nil, err
	}
	return &stats, nil
}

func (s *UserService) cacheUser(key string, u *User) {
	s.mu.Lock()
	s.cache[key] = userCacheEntry{user: u, timestamp: time.Now()}
	s.mu.Unlock()
}

func (s *UserService) invalidateCache(key string) {
	s.mu.Lock()
	delete(s.cache, key)
	s.mu.Unlock()
}
