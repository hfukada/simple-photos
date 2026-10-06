package main

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

type DB struct{ pool *pgxpool.Pool }

type User struct {
	ID    int64
	Email string
	Name  string
}

type Photo struct {
	ID          string
	UserID      int64
	OwnerName   string
	ObjectKey   string
	ThumbKey    string
	Filename    string
	ContentType string
	SizeBytes   int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

func openDB(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

func (d *DB) UpsertUser(ctx context.Context, sub, email, name string) (*User, error) {
	u := &User{Email: email, Name: name}
	err := d.pool.QueryRow(ctx, `
		INSERT INTO users (google_sub, email, name) VALUES ($1, $2, $3)
		ON CONFLICT (google_sub) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name
		RETURNING id`, sub, email, name).Scan(&u.ID)
	return u, err
}

func (d *DB) CreateSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expires)
	return err
}

func (d *DB) SessionUser(ctx context.Context, tokenHash []byte) (*User, error) {
	u := &User{}
	err := d.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`, tokenHash).Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func (d *DB) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (d *DB) InsertPhoto(ctx context.Context, p *Photo) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO photos (id, user_id, object_key, thumb_key, filename, content_type, size_bytes, created_at, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9)`,
		p.ID, p.UserID, p.ObjectKey, p.ThumbKey, p.Filename, p.ContentType, p.SizeBytes, p.CreatedAt, p.ExpiresAt)
	return err
}

const photoCols = `p.id, p.user_id, u.name, p.object_key, COALESCE(p.thumb_key, ''), p.filename,
	p.content_type, p.size_bytes, p.created_at, p.expires_at`

func scanPhoto(row pgx.Row) (*Photo, error) {
	p := &Photo{}
	err := row.Scan(&p.ID, &p.UserID, &p.OwnerName, &p.ObjectKey, &p.ThumbKey, &p.Filename,
		&p.ContentType, &p.SizeBytes, &p.CreatedAt, &p.ExpiresAt)
	return p, err
}

// ListPhotos returns unexpired photos newest first. Pass a zero cursor for the
// first page; otherwise results start strictly after (beforeTime, beforeID).
func (d *DB) ListPhotos(ctx context.Context, beforeTime time.Time, beforeID string, limit int) ([]*Photo, error) {
	if beforeTime.IsZero() {
		beforeTime = time.Now().Add(time.Hour)
	}
	rows, err := d.pool.Query(ctx, `
		SELECT `+photoCols+` FROM photos p JOIN users u ON u.id = p.user_id
		WHERE p.expires_at > now() AND (p.created_at, p.id) < ($1, $2)
		ORDER BY p.created_at DESC, p.id DESC LIMIT $3`, beforeTime, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Photo
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetPhoto(ctx context.Context, id string) (*Photo, error) {
	p, err := scanPhoto(d.pool.QueryRow(ctx, `
		SELECT `+photoCols+` FROM photos p JOIN users u ON u.id = p.user_id
		WHERE p.id = $1 AND p.expires_at > now()`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (d *DB) DeletePhoto(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM photos WHERE id = $1`, id)
	return err
}

// ExpiredPhotos returns up to limit photos whose expiry has passed.
func (d *DB) ExpiredPhotos(ctx context.Context, limit int) ([]*Photo, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+photoCols+` FROM photos p JOIN users u ON u.id = p.user_id
		WHERE p.expires_at <= now() ORDER BY p.expires_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Photo
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) DeleteExpiredSessions(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	return err
}
