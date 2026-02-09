package services

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Quote represents a row in the quotes table.
type Quote struct {
	ID        int        `json:"id"`
	QuoteText string     `json:"quote_text"`
	QuotedBy  string     `json:"quoted_by"`
	AddedBy   string     `json:"added_by"`
	AddedByID *string    `json:"added_by_id,omitempty"`
	DateSaid  *time.Time `json:"date_said,omitempty"`
	Deleted   bool       `json:"deleted"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// QuotePage is a paginated result of quotes.
type QuotePage struct {
	Quotes     []Quote `json:"quotes"`
	Total      int     `json:"total"`
	Page       int     `json:"page"`
	TotalPages int     `json:"totalPages"`
}

// QuoteService provides quote CRUD operations.
type QuoteService struct {
	db *pgxpool.Pool
}

// NewQuoteService creates a new quote service.
func NewQuoteService(db *pgxpool.Pool) *QuoteService {
	return &QuoteService{db: db}
}

// AddQuote inserts a new quote.
func (s *QuoteService) AddQuote(ctx context.Context, quoteText, quotedBy, addedBy string, addedByID *string) (*Quote, error) {
	var q Quote
	err := s.db.QueryRow(ctx,
		`INSERT INTO quotes (quote_text, quoted_by, added_by, added_by_id) VALUES ($1, $2, $3, $4) RETURNING
		 id, quote_text, quoted_by, added_by, added_by_id, date_said, deleted, created_at, updated_at`,
		quoteText, quotedBy, addedBy, addedByID,
	).Scan(&q.ID, &q.QuoteText, &q.QuotedBy, &q.AddedBy, &q.AddedByID, &q.DateSaid, &q.Deleted, &q.CreatedAt, &q.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// GetRandomQuote returns a random non-deleted quote.
func (s *QuoteService) GetRandomQuote(ctx context.Context) (*Quote, error) {
	var q Quote
	err := s.db.QueryRow(ctx,
		`SELECT id, quote_text, quoted_by, added_by, added_by_id, date_said, deleted, created_at, updated_at
		 FROM quotes WHERE deleted = false ORDER BY RANDOM() LIMIT 1`,
	).Scan(&q.ID, &q.QuoteText, &q.QuotedBy, &q.AddedBy, &q.AddedByID, &q.DateSaid, &q.Deleted, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// GetQuoteByID returns a non-deleted quote by ID.
func (s *QuoteService) GetQuoteByID(ctx context.Context, id int) (*Quote, error) {
	var q Quote
	err := s.db.QueryRow(ctx,
		`SELECT id, quote_text, quoted_by, added_by, added_by_id, date_said, deleted, created_at, updated_at
		 FROM quotes WHERE id = $1 AND deleted = false`, id,
	).Scan(&q.ID, &q.QuoteText, &q.QuotedBy, &q.AddedBy, &q.AddedByID, &q.DateSaid, &q.Deleted, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// GetAllQuotes returns paginated non-deleted quotes.
func (s *QuoteService) GetAllQuotes(ctx context.Context, page, limit int) (*QuotePage, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}
	offset := (page - 1) * limit

	var total int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM quotes WHERE deleted = false`).Scan(&total)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, quote_text, quoted_by, added_by, added_by_id, date_said, deleted, created_at, updated_at
		 FROM quotes WHERE deleted = false ORDER BY id DESC LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var quotes []Quote
	for rows.Next() {
		var q Quote
		if err := rows.Scan(&q.ID, &q.QuoteText, &q.QuotedBy, &q.AddedBy, &q.AddedByID, &q.DateSaid, &q.Deleted, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		quotes = append(quotes, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	totalPages := (total + limit - 1) / limit
	return &QuotePage{Quotes: quotes, Total: total, Page: page, TotalPages: totalPages}, nil
}

// SearchQuotes searches quotes by text or author (case-insensitive).
func (s *QuoteService) SearchQuotes(ctx context.Context, searchTerm string, page, limit int) (*QuotePage, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}
	offset := (page - 1) * limit
	pattern := "%" + searchTerm + "%"

	var total int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM quotes WHERE deleted = false AND (quote_text ILIKE $1 OR quoted_by ILIKE $1)`,
		pattern,
	).Scan(&total)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, quote_text, quoted_by, added_by, added_by_id, date_said, deleted, created_at, updated_at
		 FROM quotes WHERE deleted = false AND (quote_text ILIKE $1 OR quoted_by ILIKE $1)
		 ORDER BY id DESC LIMIT $2 OFFSET $3`,
		pattern, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var quotes []Quote
	for rows.Next() {
		var q Quote
		if err := rows.Scan(&q.ID, &q.QuoteText, &q.QuotedBy, &q.AddedBy, &q.AddedByID, &q.DateSaid, &q.Deleted, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		quotes = append(quotes, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	totalPages := (total + limit - 1) / limit
	return &QuotePage{Quotes: quotes, Total: total, Page: page, TotalPages: totalPages}, nil
}

// DeleteQuote soft-deletes a quote by ID.
func (s *QuoteService) DeleteQuote(ctx context.Context, id int) (*Quote, error) {
	var q Quote
	err := s.db.QueryRow(ctx,
		`UPDATE quotes SET deleted = true, updated_at = CURRENT_TIMESTAMP WHERE id = $1
		 RETURNING id, quote_text, quoted_by, added_by, added_by_id, date_said, deleted, created_at, updated_at`, id,
	).Scan(&q.ID, &q.QuoteText, &q.QuotedBy, &q.AddedBy, &q.AddedByID, &q.DateSaid, &q.Deleted, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// UpdateQuote updates the text and author of a non-deleted quote.
func (s *QuoteService) UpdateQuote(ctx context.Context, id int, quoteText, quotedBy string) (*Quote, error) {
	var q Quote
	err := s.db.QueryRow(ctx,
		`UPDATE quotes SET quote_text = $2, quoted_by = $3, updated_at = CURRENT_TIMESTAMP
		 WHERE id = $1 AND deleted = false
		 RETURNING id, quote_text, quoted_by, added_by, added_by_id, date_said, deleted, created_at, updated_at`,
		id, quoteText, quotedBy,
	).Scan(&q.ID, &q.QuoteText, &q.QuotedBy, &q.AddedBy, &q.AddedByID, &q.DateSaid, &q.Deleted, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// GetQuoteCount returns the number of non-deleted quotes.
func (s *QuoteService) GetQuoteCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM quotes WHERE deleted = false`).Scan(&count)
	return count, err
}

// GetQuotesByUser returns paginated quotes by a specific author.
func (s *QuoteService) GetQuotesByUser(ctx context.Context, username string, page, limit int) (*QuotePage, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}
	offset := (page - 1) * limit

	var total int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM quotes WHERE deleted = false AND quoted_by = $1`,
		username,
	).Scan(&total)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, quote_text, quoted_by, added_by, added_by_id, date_said, deleted, created_at, updated_at
		 FROM quotes WHERE deleted = false AND quoted_by = $1 ORDER BY id DESC LIMIT $2 OFFSET $3`,
		username, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var quotes []Quote
	for rows.Next() {
		var q Quote
		if err := rows.Scan(&q.ID, &q.QuoteText, &q.QuotedBy, &q.AddedBy, &q.AddedByID, &q.DateSaid, &q.Deleted, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		quotes = append(quotes, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	totalPages := (total + limit - 1) / limit
	return &QuotePage{Quotes: quotes, Total: total, Page: page, TotalPages: totalPages}, nil
}
