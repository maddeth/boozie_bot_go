package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EggUser represents a row in the eggs table.
type EggUser struct {
	ID                int       `json:"id"`
	Username          string    `json:"username"`
	UsernameSanitised string    `json:"username_sanitised"`
	TwitchUserID      *string   `json:"twitch_user_id"`
	EggsAmount        int       `json:"eggs_amount"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// EggStats contains aggregate egg statistics.
type EggStats struct {
	TotalUsers  int     `json:"totalUsers"`
	TotalEggs   int     `json:"totalEggs"`
	AverageEggs float64 `json:"averageEggs"`
	MaxEggs     int     `json:"maxEggs"`
}

// EggService provides egg (points) CRUD operations.
type EggService struct {
	db *pgxpool.Pool
}

// NewEggService creates a new egg service.
func NewEggService(db *pgxpool.Pool) *EggService {
	return &EggService{db: db}
}

const eggSelectCols = `id, username, username_sanitised, twitch_user_id, eggs_amount, created_at, updated_at`

func scanEggUser(row pgx.Row) (*EggUser, error) {
	var e EggUser
	err := row.Scan(&e.ID, &e.Username, &e.UsernameSanitised, &e.TwitchUserID, &e.EggsAmount, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// GetUserEggs looks up a user's eggs by Twitch ID, falling back to username.
func (s *EggService) GetUserEggs(ctx context.Context, identifier string) (*EggUser, error) {
	// Try by twitch_user_id first
	e, err := scanEggUser(s.db.QueryRow(ctx,
		`SELECT `+eggSelectCols+` FROM eggs WHERE twitch_user_id = $1`, identifier,
	))
	if err != nil {
		return nil, err
	}
	if e != nil {
		return e, nil
	}

	// Fallback to username
	e, err = scanEggUser(s.db.QueryRow(ctx,
		`SELECT `+eggSelectCols+` FROM eggs WHERE LOWER(username_sanitised) = LOWER($1) OR LOWER(username) = LOWER($1)`,
		identifier,
	))
	return e, err
}

// CreateOrUpdateUserEggs creates a new egg record or updates an existing one.
func (s *EggService) CreateOrUpdateUserEggs(ctx context.Context, twitchUserID, username string, eggAmount int) (*EggUser, error) {
	existing, err := s.GetUserEggs(ctx, twitchUserID)
	if err != nil {
		return nil, err
	}

	sanitised := strings.ToLower(username)

	if existing != nil {
		// Update username if needed
		return scanEggUser(s.db.QueryRow(ctx,
			`UPDATE eggs SET username = $2, username_sanitised = $3, updated_at = NOW()
			 WHERE id = $1 RETURNING `+eggSelectCols,
			existing.ID, username, sanitised,
		))
	}

	// Create new
	return scanEggUser(s.db.QueryRow(ctx,
		`INSERT INTO eggs (twitch_user_id, username, username_sanitised, eggs_amount)
		 VALUES ($1, $2, $3, $4) RETURNING `+eggSelectCols,
		twitchUserID, username, sanitised, eggAmount,
	))
}

// ErrInsufficientEggs is returned when a user tries to spend more eggs than they have.
var ErrInsufficientEggs = fmt.Errorf("insufficient eggs")

// UpdateUserEggs adds or removes eggs from a user. Uses a transaction for safety.
// Returns ErrInsufficientEggs if the user doesn't have enough.
func (s *EggService) UpdateUserEggs(ctx context.Context, twitchUserID, username string, eggChange int) (*EggUser, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the row for update
	var e EggUser
	err = tx.QueryRow(ctx,
		`SELECT `+eggSelectCols+` FROM eggs WHERE twitch_user_id = $1 FOR UPDATE`, twitchUserID,
	).Scan(&e.ID, &e.Username, &e.UsernameSanitised, &e.TwitchUserID, &e.EggsAmount, &e.CreatedAt, &e.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		if eggChange <= 0 {
			return nil, ErrInsufficientEggs
		}
		// Create user with initial eggs
		err = tx.QueryRow(ctx,
			`INSERT INTO eggs (twitch_user_id, username, username_sanitised, eggs_amount)
			 VALUES ($1, $2, $3, $4) RETURNING `+eggSelectCols,
			twitchUserID, username, strings.ToLower(username), eggChange,
		).Scan(&e.ID, &e.Username, &e.UsernameSanitised, &e.TwitchUserID, &e.EggsAmount, &e.CreatedAt, &e.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &e, nil
	}
	if err != nil {
		return nil, err
	}

	newAmount := e.EggsAmount + eggChange
	if newAmount < 0 {
		return nil, ErrInsufficientEggs
	}

	err = tx.QueryRow(ctx,
		`UPDATE eggs SET eggs_amount = $1, username = $2, username_sanitised = $3, updated_at = NOW()
		 WHERE id = $4 RETURNING `+eggSelectCols,
		newAmount, username, strings.ToLower(username), e.ID,
	).Scan(&e.ID, &e.Username, &e.UsernameSanitised, &e.TwitchUserID, &e.EggsAmount, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &e, nil
}

// GetAll returns all egg users, ordered by eggs_amount DESC or username ASC.
func (s *EggService) GetAll(ctx context.Context, limit int, orderByUsername bool) ([]EggUser, error) {
	if limit <= 0 {
		limit = 100
	}

	orderClause := "eggs_amount DESC"
	if orderByUsername {
		orderClause = "username ASC"
	}

	rows, err := s.db.Query(ctx,
		`SELECT `+eggSelectCols+` FROM eggs ORDER BY `+orderClause+` LIMIT $1`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []EggUser
	for rows.Next() {
		var e EggUser
		if err := rows.Scan(&e.ID, &e.Username, &e.UsernameSanitised, &e.TwitchUserID, &e.EggsAmount, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, e)
	}
	return users, rows.Err()
}

// LeaderboardEntry is a simplified egg record for the leaderboard.
type LeaderboardEntry struct {
	Username     string    `json:"username"`
	EggsAmount   int       `json:"eggs_amount"`
	TwitchUserID *string   `json:"twitch_user_id"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// GetLeaderboard returns the top egg holders.
func (s *EggService) GetLeaderboard(ctx context.Context, limit int) ([]LeaderboardEntry, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := s.db.Query(ctx,
		`SELECT username, eggs_amount, twitch_user_id, updated_at
		 FROM eggs WHERE eggs_amount > 0 ORDER BY eggs_amount DESC LIMIT $1`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		if err := rows.Scan(&e.Username, &e.EggsAmount, &e.TwitchUserID, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// GetStats returns aggregate egg statistics.
func (s *EggService) GetStats(ctx context.Context) (*EggStats, error) {
	var stats EggStats
	var avgEggs *float64
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(eggs_amount), 0), AVG(eggs_amount), COALESCE(MAX(eggs_amount), 0) FROM eggs`,
	).Scan(&stats.TotalUsers, &stats.TotalEggs, &avgEggs, &stats.MaxEggs)
	if err != nil {
		return nil, err
	}
	if avgEggs != nil {
		stats.AverageEggs = *avgEggs
	}
	return &stats, nil
}

// GetUserRank returns a user's rank based on their egg count (1-indexed).
func (s *EggService) GetUserRank(ctx context.Context, eggsAmount int) (int, error) {
	var rank int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) + 1 FROM eggs WHERE eggs_amount > $1`, eggsAmount,
	).Scan(&rank)
	return rank, err
}

// EggUpdateCommand is a helper for the chat !add<points> / !remove<points> commands.
// It calls UpdateUserEggs and returns a formatted chat message.
// pointsName/pointsNameSingular control the user-facing name (e.g. "eggs"/"egg").
func (s *EggService) EggUpdateCommand(ctx context.Context, userToUpdate string, eggsToAdd int, twitchUserID, pointsName, pointsNameSingular string) (string, error) {
	result, err := s.UpdateUserEggs(ctx, twitchUserID, userToUpdate, eggsToAdd)
	if errors.Is(err, ErrInsufficientEggs) {
		return fmt.Sprintf("%s doesn't have enough %s!", userToUpdate, pointsName), nil
	}
	if err != nil {
		slog.Error("egg update command failed", "error", err, "user", userToUpdate)
		return "", err
	}

	word := pointsName
	if abs(eggsToAdd) == 1 {
		word = pointsNameSingular
	}

	if eggsToAdd >= 0 {
		return fmt.Sprintf("%s gained %d %s! (Total: %d)", userToUpdate, eggsToAdd, word, result.EggsAmount), nil
	}
	return fmt.Sprintf("%s lost %d %s! (Total: %d)", userToUpdate, -eggsToAdd, word, result.EggsAmount), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
