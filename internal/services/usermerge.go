package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MergeLog represents a row in the user_merge_log table.
type MergeLog struct {
	ID              int       `json:"id"`
	SourceUserID    string    `json:"source_user_id"`
	SourceUsername   string    `json:"source_username"`
	TargetUserID    string    `json:"target_user_id"`
	TargetUsername   string    `json:"target_username"`
	EggsTransferred int       `json:"eggs_transferred"`
	AdminTwitchID   string    `json:"admin_twitch_id"`
	AdminUsername    string    `json:"admin_username"`
	Reason          *string   `json:"reason"`
	MergeDate       time.Time `json:"merge_date"`
}

// MergePreview is a read-only preview of what a merge would do.
type MergePreview struct {
	Source     *EggUser `json:"source"`
	Target     *EggUser `json:"target"`
	ResultEggs int      `json:"resultEggs"`
}

// UserMergeService handles merging egg accounts.
type UserMergeService struct {
	db *pgxpool.Pool
}

// NewUserMergeService creates a new user merge service.
func NewUserMergeService(db *pgxpool.Pool) *UserMergeService {
	return &UserMergeService{db: db}
}

// isNumeric checks if a string is all digits (likely a Twitch user ID).
func isNumeric(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

// getEggUser looks up an egg user by twitch ID or username.
func (s *UserMergeService) getEggUser(ctx context.Context, tx pgx.Tx, identifier string) (*EggUser, error) {
	var e EggUser

	// Try by twitch_user_id first
	err := tx.QueryRow(ctx,
		`SELECT `+eggSelectCols+` FROM eggs WHERE twitch_user_id = $1`, identifier,
	).Scan(&e.ID, &e.Username, &e.UsernameSanitised, &e.TwitchUserID, &e.EggsAmount, &e.CreatedAt, &e.UpdatedAt)
	if err == nil {
		return &e, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Fallback to username
	err = tx.QueryRow(ctx,
		`SELECT `+eggSelectCols+` FROM eggs WHERE LOWER(username_sanitised) = LOWER($1)`, identifier,
	).Scan(&e.ID, &e.Username, &e.UsernameSanitised, &e.TwitchUserID, &e.EggsAmount, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// createEggUser creates a new egg user row. Uses twitch_user_id if identifier is numeric, otherwise username.
func (s *UserMergeService) createEggUser(ctx context.Context, tx pgx.Tx, identifier string) error {
	if isNumeric(identifier) {
		_, err := tx.Exec(ctx,
			`INSERT INTO eggs (twitch_user_id, username, username_sanitised, eggs_amount) VALUES ($1, $1, $1, 0)`,
			identifier,
		)
		return err
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO eggs (username, username_sanitised, eggs_amount) VALUES ($1, $1, 0)`,
		identifier,
	)
	return err
}

// PreviewMerge shows what a merge would do without making changes.
func (s *UserMergeService) PreviewMerge(ctx context.Context, fromUserID, toUserID string) (*MergePreview, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	source, err := s.getEggUser(ctx, tx, fromUserID)
	if err != nil {
		return nil, err
	}
	target, err := s.getEggUser(ctx, tx, toUserID)
	if err != nil {
		return nil, err
	}

	sourceEggs := 0
	if source != nil {
		sourceEggs = source.EggsAmount
	}
	targetEggs := 0
	if target != nil {
		targetEggs = target.EggsAmount
	}

	return &MergePreview{
		Source:     source,
		Target:     target,
		ResultEggs: sourceEggs + targetEggs,
	}, nil
}

// MergeUserEggs transfers all eggs from source to target. Uses a transaction.
func (s *UserMergeService) MergeUserEggs(ctx context.Context, fromUserID, toUserID, adminTwitchID, adminUsername string, reason *string, deleteSource bool) (*MergeLog, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Get source
	source, err := s.getEggUser(ctx, tx, fromUserID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("source user %q not found", fromUserID)
	}

	// Get or create target
	target, err := s.getEggUser(ctx, tx, toUserID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		if err := s.createEggUser(ctx, tx, toUserID); err != nil {
			return nil, fmt.Errorf("creating target user: %w", err)
		}
		target, err = s.getEggUser(ctx, tx, toUserID)
		if err != nil || target == nil {
			return nil, fmt.Errorf("failed to get newly created target user")
		}
	}

	// Transfer eggs
	newTargetAmount := target.EggsAmount + source.EggsAmount
	_, err = tx.Exec(ctx, `UPDATE eggs SET eggs_amount = $1 WHERE id = $2`, newTargetAmount, target.ID)
	if err != nil {
		return nil, err
	}

	// Log the merge
	var log MergeLog
	err = tx.QueryRow(ctx,
		`INSERT INTO user_merge_log (source_user_id, source_username, target_user_id, target_username,
		 eggs_transferred, admin_twitch_id, admin_username, reason, merge_date)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		 RETURNING id, source_user_id, source_username, target_user_id, target_username,
		 eggs_transferred, admin_twitch_id, admin_username, reason, merge_date`,
		fromUserID, source.Username, toUserID, target.Username,
		source.EggsAmount, adminTwitchID, adminUsername, reason,
	).Scan(&log.ID, &log.SourceUserID, &log.SourceUsername, &log.TargetUserID, &log.TargetUsername,
		&log.EggsTransferred, &log.AdminTwitchID, &log.AdminUsername, &log.Reason, &log.MergeDate)
	if err != nil {
		return nil, err
	}

	// Handle source cleanup
	if deleteSource {
		_, err = tx.Exec(ctx, `DELETE FROM eggs WHERE id = $1`, source.ID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE eggs SET eggs_amount = 0 WHERE id = $1`, source.ID)
	}
	if err != nil {
		return nil, err
	}

	// Update pool_transactions to point to new user
	if source.TwitchUserID != nil {
		_, err = tx.Exec(ctx,
			`UPDATE pool_transactions SET donor_twitch_id = $1, donor_username = $2 WHERE donor_twitch_id = $3`,
			toUserID, target.Username, *source.TwitchUserID,
		)
		if err != nil {
			slog.Warn("failed to update pool transactions during merge", "error", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	slog.Info("egg merge completed",
		"source", fromUserID,
		"target", toUserID,
		"eggs_transferred", source.EggsAmount,
		"admin", adminUsername,
	)

	return &log, nil
}

// GetMergeHistory returns merge history involving a specific user.
func (s *UserMergeService) GetMergeHistory(ctx context.Context, userID string, limit int) ([]MergeLog, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, source_user_id, source_username, target_user_id, target_username,
		 eggs_transferred, admin_twitch_id, admin_username, reason, merge_date
		 FROM user_merge_log
		 WHERE source_user_id = $1 OR target_user_id = $1 OR source_username = $1 OR target_username = $1
		 ORDER BY merge_date DESC LIMIT $2`,
		userID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanMergeLogs(rows)
}

// GetAllMergeHistory returns all merge history.
func (s *UserMergeService) GetAllMergeHistory(ctx context.Context, limit int) ([]MergeLog, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, source_user_id, source_username, target_user_id, target_username,
		 eggs_transferred, admin_twitch_id, admin_username, reason, merge_date
		 FROM user_merge_log ORDER BY merge_date DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanMergeLogs(rows)
}

func scanMergeLogs(rows pgx.Rows) ([]MergeLog, error) {
	var logs []MergeLog
	for rows.Next() {
		var l MergeLog
		if err := rows.Scan(&l.ID, &l.SourceUserID, &l.SourceUsername, &l.TargetUserID, &l.TargetUsername,
			&l.EggsTransferred, &l.AdminTwitchID, &l.AdminUsername, &l.Reason, &l.MergeDate); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
