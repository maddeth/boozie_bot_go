package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Colour represents a row in the colours table.
type Colour struct {
	ID         int    `json:"id"`
	ColourName string `json:"colourname"`
	HexValue   string `json:"hex_value"`
	Username   string `json:"username"`
}

// ColourService provides colour CRUD operations.
type ColourService struct {
	db *pgxpool.Pool
}

// NewColourService creates a new colour service.
func NewColourService(db *pgxpool.Pool) *ColourService {
	return &ColourService{db: db}
}

// GetCount returns the total number of colours.
func (s *ColourService) GetCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM colours`).Scan(&count)
	return count, err
}

// GetRandomByName returns a random colour matching the given name (case-insensitive partial match).
func (s *ColourService) GetRandomByName(ctx context.Context, name string) (string, error) {
	var colourName string
	err := s.db.QueryRow(ctx,
		`SELECT colourname FROM colours WHERE colourname ILIKE $1 ORDER BY RANDOM() LIMIT 1`,
		"%"+name+"%",
	).Scan(&colourName)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return colourName, err
}

// GetAll returns all colours.
func (s *ColourService) GetAll(ctx context.Context) ([]Colour, error) {
	rows, err := s.db.Query(ctx, `SELECT id, colourname, hex_value, username FROM colours`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var colours []Colour
	for rows.Next() {
		var c Colour
		if err := rows.Scan(&c.ID, &c.ColourName, &c.HexValue, &c.Username); err != nil {
			return nil, err
		}
		colours = append(colours, c)
	}
	return colours, rows.Err()
}

// GetByID returns a colour by its ID.
func (s *ColourService) GetByID(ctx context.Context, id int) (*Colour, error) {
	var c Colour
	err := s.db.QueryRow(ctx,
		`SELECT id, colourname, hex_value, username FROM colours WHERE id = $1`, id,
	).Scan(&c.ID, &c.ColourName, &c.HexValue, &c.Username)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SearchByName returns colours matching the name (case-insensitive partial match).
func (s *ColourService) SearchByName(ctx context.Context, name string) ([]Colour, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, colourname, hex_value, username FROM colours WHERE colourname ILIKE $1`,
		"%"+name+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var colours []Colour
	for rows.Next() {
		var c Colour
		if err := rows.Scan(&c.ID, &c.ColourName, &c.HexValue, &c.Username); err != nil {
			return nil, err
		}
		colours = append(colours, c)
	}
	return colours, rows.Err()
}

// GetByHex returns colour names matching the given hex value.
func (s *ColourService) GetByHex(ctx context.Context, hex string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT colourname FROM colours WHERE hex_value = $1`, hex,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// GetHexByName returns the hex value for an exact colour name match.
func (s *ColourService) GetHexByName(ctx context.Context, name string) (string, error) {
	var hex string
	err := s.db.QueryRow(ctx,
		`SELECT hex_value FROM colours WHERE colourname = $1`, name,
	).Scan(&hex)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return hex, err
}

// GetByUsername returns all colours added by a specific user.
func (s *ColourService) GetByUsername(ctx context.Context, username string) ([]Colour, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, colourname, hex_value, username FROM colours WHERE username = $1`, username,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var colours []Colour
	for rows.Next() {
		var c Colour
		if err := rows.Scan(&c.ID, &c.ColourName, &c.HexValue, &c.Username); err != nil {
			return nil, err
		}
		colours = append(colours, c)
	}
	return colours, rows.Err()
}

// GetLast returns the most recently added colour.
func (s *ColourService) GetLast(ctx context.Context) (*Colour, error) {
	var c Colour
	err := s.db.QueryRow(ctx,
		`SELECT id, colourname, hex_value, username FROM colours ORDER BY id DESC LIMIT 1`,
	).Scan(&c.ID, &c.ColourName, &c.HexValue, &c.Username)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Add inserts a new colour. Returns an error if the colour already exists.
func (s *ColourService) Add(ctx context.Context, colourName, hex, username string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO colours (colourname, hex_value, username) VALUES ($1, $2, $3)`,
		colourName, hex, username,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("colour %q already exists", colourName)
		}
		slog.Error("failed to add colour", "error", err, "colour", colourName)
		return err
	}
	return nil
}

// Rename updates the name and/or hex value of an existing colour by ID.
// Returns pgx.ErrNoRows if the colour does not exist.
func (s *ColourService) Rename(ctx context.Context, id int, newName, newHex string) error {
	if newName != "" && newHex != "" {
		_, err := s.db.Exec(ctx,
			`UPDATE colours SET colourname = $2, hex_value = $3 WHERE id = $1`,
			id, newName, newHex,
		)
		return err
	} else if newName != "" {
		_, err := s.db.Exec(ctx,
			`UPDATE colours SET colourname = $2 WHERE id = $1`,
			id, newName,
		)
		return err
	} else if newHex != "" {
		_, err := s.db.Exec(ctx,
			`UPDATE colours SET hex_value = $2 WHERE id = $1`,
			id, newHex,
		)
		return err
	}
	return nil
}

// Delete removes a colour by ID.
func (s *ColourService) Delete(ctx context.Context, id int) error {
	_, err := s.db.Exec(ctx, `DELETE FROM colours WHERE id = $1`, id)
	return err
}
