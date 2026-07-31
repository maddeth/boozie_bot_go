package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var poolNameRegex = regexp.MustCompile(`[^a-z0-9_]`)

// Pool represents a row in the pools table.
type Pool struct {
	ID                 int       `json:"id"`
	PoolName           string    `json:"pool_name"`
	PoolNameSanitised  string    `json:"pool_name_sanitised"`
	Description        *string   `json:"description"`
	EggsAmount         int       `json:"eggs_amount"`
	IsActive           bool      `json:"is_active"`
	CreatedByTwitchID  string    `json:"created_by_twitch_id"`
	CreatedByUsername   string    `json:"created_by_username"`
	CreatedAt          time.Time `json:"created_at"`
	UniqueDonors       int       `json:"unique_donors"`
	TotalDonations     int       `json:"total_donations"`
}

// PoolTransaction represents a row in the pool_transactions table.
type PoolTransaction struct {
	ID              int       `json:"id"`
	PoolID          int       `json:"pool_id"`
	DonorTwitchID   *string   `json:"donor_twitch_id"`
	DonorUsername   *string   `json:"donor_username"`
	EggsAmount      int       `json:"eggs_amount"`
	TransactionType string    `json:"transaction_type"`
	Notes           *string   `json:"notes"`
	CreatedAt       time.Time `json:"created_at"`
}

// PoolService provides pool CRUD operations with transactions.
type PoolService struct {
	db *pgxpool.Pool
}

// NewPoolService creates a new pool service.
func NewPoolService(db *pgxpool.Pool) *PoolService {
	return &PoolService{db: db}
}

// sanitizePoolName normalises a pool name to lowercase alphanumeric + underscore.
func sanitizePoolName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = poolNameRegex.ReplaceAllString(s, "_")
	if !strings.HasPrefix(s, "pool_") {
		s = "pool_" + s
	}
	return s
}

// CreatePool creates a new pool or reactivates an inactive one.
func (s *PoolService) CreatePool(ctx context.Context, poolName, description, creatorTwitchID, creatorUsername string) (*Pool, error) {
	sanitised := sanitizePoolName(poolName)

	// Check if pool exists
	var existingID int
	var isActive bool
	err := s.db.QueryRow(ctx,
		`SELECT id, is_active FROM pools WHERE pool_name_sanitised = $1`, sanitised,
	).Scan(&existingID, &isActive)

	if err == nil {
		if isActive {
			return nil, fmt.Errorf("pool %q already exists", sanitised)
		}
		// Reactivate
		var p Pool
		err = s.db.QueryRow(ctx,
			`UPDATE pools SET is_active = true, description = $2, created_by_twitch_id = $3, created_by_username = $4
			 WHERE id = $1 RETURNING id, pool_name, pool_name_sanitised, description, eggs_amount, is_active,
			 created_by_twitch_id, created_by_username, created_at`,
			existingID, description, creatorTwitchID, creatorUsername,
		).Scan(&p.ID, &p.PoolName, &p.PoolNameSanitised, &p.Description, &p.EggsAmount, &p.IsActive,
			&p.CreatedByTwitchID, &p.CreatedByUsername, &p.CreatedAt)
		if err != nil {
			return nil, err
		}
		return &p, nil
	}

	// Create new
	var p Pool
	err = s.db.QueryRow(ctx,
		`INSERT INTO pools (pool_name, pool_name_sanitised, description, created_by_twitch_id, created_by_username)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, pool_name, pool_name_sanitised, description, eggs_amount, is_active,
		 created_by_twitch_id, created_by_username, created_at`,
		poolName, sanitised, description, creatorTwitchID, creatorUsername,
	).Scan(&p.ID, &p.PoolName, &p.PoolNameSanitised, &p.Description, &p.EggsAmount, &p.IsActive,
		&p.CreatedByTwitchID, &p.CreatedByUsername, &p.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, fmt.Errorf("pool %q already exists", sanitised)
		}
		return nil, err
	}
	return &p, nil
}

// DonateToPool transfers eggs from a user to a pool. Uses a transaction.
func (s *PoolService) DonateToPool(ctx context.Context, poolName, donorTwitchID, donorUsername string, amount int) (*Pool, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("donation amount must be positive")
	}

	sanitised := sanitizePoolName(poolName)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Get pool (locked)
	var poolID, poolEggs int
	var poolActive bool
	err = tx.QueryRow(ctx,
		`SELECT id, eggs_amount, is_active FROM pools WHERE pool_name_sanitised = $1 FOR UPDATE`, sanitised,
	).Scan(&poolID, &poolEggs, &poolActive)
	if err != nil {
		return nil, fmt.Errorf("pool %q not found", sanitised)
	}
	if !poolActive {
		return nil, fmt.Errorf("pool %q is not active", sanitised)
	}

	// Check donor's egg balance
	var donorEggs int
	err = tx.QueryRow(ctx,
		`SELECT eggs_amount FROM eggs WHERE twitch_user_id = $1 FOR UPDATE`, donorTwitchID,
	).Scan(&donorEggs)
	if err != nil {
		return nil, fmt.Errorf("donor not found or has no eggs")
	}
	if donorEggs < amount {
		return nil, fmt.Errorf("insufficient eggs (have %d, need %d)", donorEggs, amount)
	}

	// Deduct from donor
	_, err = tx.Exec(ctx,
		`UPDATE eggs SET eggs_amount = eggs_amount - $1 WHERE twitch_user_id = $2`,
		amount, donorTwitchID,
	)
	if err != nil {
		return nil, err
	}

	// Add to pool
	_, err = tx.Exec(ctx,
		`UPDATE pools SET eggs_amount = eggs_amount + $1 WHERE id = $2`,
		amount, poolID,
	)
	if err != nil {
		return nil, err
	}

	// Record transaction
	_, err = tx.Exec(ctx,
		`INSERT INTO pool_transactions (pool_id, donor_twitch_id, donor_username, eggs_amount, transaction_type)
		 VALUES ($1, $2, $3, $4, 'donation')`,
		poolID, donorTwitchID, donorUsername, amount,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	slog.Info("pool donation", "pool", sanitised, "donor", donorUsername, "amount", amount)

	// Return updated pool
	return s.GetPool(ctx, poolName)
}

// GetPool returns a pool with donor/donation stats.
func (s *PoolService) GetPool(ctx context.Context, poolName string) (*Pool, error) {
	sanitised := sanitizePoolName(poolName)

	var p Pool
	err := s.db.QueryRow(ctx,
		`SELECT p.id, p.pool_name, p.pool_name_sanitised, p.description, p.eggs_amount, p.is_active,
			p.created_by_twitch_id, p.created_by_username, p.created_at,
			COUNT(DISTINCT pt.donor_twitch_id), COUNT(pt.id)
		 FROM pools p
		 LEFT JOIN pool_transactions pt ON pt.pool_id = p.id
		 WHERE p.pool_name_sanitised = $1
		 GROUP BY p.id`, sanitised,
	).Scan(&p.ID, &p.PoolName, &p.PoolNameSanitised, &p.Description, &p.EggsAmount, &p.IsActive,
		&p.CreatedByTwitchID, &p.CreatedByUsername, &p.CreatedAt, &p.UniqueDonors, &p.TotalDonations)

	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetAllPools returns all active pools with donor/donation stats.
func (s *PoolService) GetAllPools(ctx context.Context) ([]Pool, error) {
	rows, err := s.db.Query(ctx,
		`SELECT p.id, p.pool_name, p.pool_name_sanitised, p.description, p.eggs_amount, p.is_active,
			p.created_by_twitch_id, p.created_by_username, p.created_at,
			COUNT(DISTINCT pt.donor_twitch_id), COUNT(pt.id)
		 FROM pools p
		 LEFT JOIN pool_transactions pt ON pt.pool_id = p.id
		 WHERE p.is_active = true
		 GROUP BY p.id
		 ORDER BY p.eggs_amount DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pools []Pool
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.ID, &p.PoolName, &p.PoolNameSanitised, &p.Description, &p.EggsAmount, &p.IsActive,
			&p.CreatedByTwitchID, &p.CreatedByUsername, &p.CreatedAt, &p.UniqueDonors, &p.TotalDonations); err != nil {
			return nil, err
		}
		pools = append(pools, p)
	}
	return pools, rows.Err()
}

// GetRecentDonations returns recent transactions for a pool.
func (s *PoolService) GetRecentDonations(ctx context.Context, poolName string, limit int) ([]PoolTransaction, error) {
	if limit <= 0 {
		limit = 10
	}
	sanitised := sanitizePoolName(poolName)

	rows, err := s.db.Query(ctx,
		`SELECT pt.id, pt.pool_id, pt.donor_twitch_id, pt.donor_username, pt.eggs_amount,
			pt.transaction_type, pt.notes, pt.created_at
		 FROM pool_transactions pt
		 JOIN pools p ON pt.pool_id = p.id
		 WHERE p.pool_name_sanitised = $1
		 ORDER BY pt.created_at DESC LIMIT $2`,
		sanitised, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txns []PoolTransaction
	for rows.Next() {
		var t PoolTransaction
		if err := rows.Scan(&t.ID, &t.PoolID, &t.DonorTwitchID, &t.DonorUsername, &t.EggsAmount,
			&t.TransactionType, &t.Notes, &t.CreatedAt); err != nil {
			return nil, err
		}
		txns = append(txns, t)
	}
	return txns, rows.Err()
}

// DeletePool soft-deletes a pool.
func (s *PoolService) DeletePool(ctx context.Context, poolName string) error {
	sanitised := sanitizePoolName(poolName)
	_, err := s.db.Exec(ctx,
		`UPDATE pools SET is_active = false WHERE pool_name_sanitised = $1`, sanitised,
	)
	return err
}

// AdminAdjustPool adjusts a pool's egg balance (admin action). Uses a transaction.
func (s *PoolService) AdminAdjustPool(ctx context.Context, poolName string, amount int, adminTwitchID, adminUsername string, notes *string) (*Pool, error) {
	sanitised := sanitizePoolName(poolName)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var poolID, currentEggs int
	err = tx.QueryRow(ctx,
		`SELECT id, eggs_amount FROM pools WHERE pool_name_sanitised = $1 AND is_active = true FOR UPDATE`,
		sanitised,
	).Scan(&poolID, &currentEggs)
	if err != nil {
		return nil, fmt.Errorf("pool %q not found or inactive", sanitised)
	}

	newAmount := currentEggs + amount
	if newAmount < 0 {
		return nil, fmt.Errorf("adjustment would result in negative balance")
	}

	_, err = tx.Exec(ctx,
		`UPDATE pools SET eggs_amount = $1 WHERE id = $2`, newAmount, poolID,
	)
	if err != nil {
		return nil, err
	}

	txType := "admin_add"
	if amount < 0 {
		txType = "admin_remove"
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO pool_transactions (pool_id, donor_twitch_id, donor_username, eggs_amount, transaction_type, notes)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		poolID, adminTwitchID, adminUsername, abs(amount), txType, notes,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return s.GetPool(ctx, poolName)
}
