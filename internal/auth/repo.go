package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

const uniqueViolation = "23505"

// CreateUser inserts the user and an empty prefs row in one transaction, so
// every user is guaranteed to have prefs.
func (r *Repo) CreateUser(ctx context.Context, u User) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3)`,
		u.ID, u.Email, u.PasswordHash)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return fmt.Errorf("%w: email already registered", httpx.ErrConflict)
		}
		return fmt.Errorf("insert user: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_prefs (user_id) VALUES ($1)`, u.ID); err != nil {
		return fmt.Errorf("insert user prefs: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func (r *Repo) UserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := r.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE lower(email) = lower($1)`,
		email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, httpx.ErrNotFound
	}
	if err != nil {
		return u, fmt.Errorf("select user by email: %w", err)
	}
	return u, nil
}

func (r *Repo) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	err := r.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, httpx.ErrNotFound
	}
	if err != nil {
		return u, fmt.Errorf("select user by id: %w", err)
	}
	return u, nil
}

// UserExists lets the access-token middleware reject tokens of a deleted
// account: access tokens are not revocable and outlive the user by up to
// JWT_ACCESS_TTL.
func (r *Repo) UserExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, id).Scan(&exists); err != nil {
		return false, fmt.Errorf("check user exists: %w", err)
	}
	return exists, nil
}

// DeleteUser removes the account. Every per-user table references users with
// ON DELETE CASCADE, so prefs, fridge, refresh tokens, history and generation
// runs go with it.
func (r *Repo) DeleteUser(ctx context.Context, id uuid.UUID) error {
	if _, err := r.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}

func (r *Repo) StoreRefreshToken(ctx context.Context, digest []byte, userID uuid.UUID, expiresAt time.Time) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO refresh_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		digest, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("insert refresh token: %w", err)
	}
	return nil
}

// ConsumeRefreshToken atomically revokes a live refresh token and returns its
// owner. Replaying a token that was already used yields ErrUnauthorized.
func (r *Repo) ConsumeRefreshToken(ctx context.Context, digest []byte) (uuid.UUID, error) {
	var userID uuid.UUID
	err := r.pool.QueryRow(ctx,
		`UPDATE refresh_tokens SET revoked_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
		 RETURNING user_id`,
		digest,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("%w: refresh token is invalid or expired", httpx.ErrUnauthorized)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("consume refresh token: %w", err)
	}
	return userID, nil
}

// RevokeRefreshToken revokes the token only if it belongs to userID. An unknown,
// foreign, expired or already revoked token is not an error: logout answers the
// same either way, so it reveals nothing about tokens it does not own.
func (r *Repo) RevokeRefreshToken(ctx context.Context, digest []byte, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now()
		 WHERE token_hash = $1 AND user_id = $2 AND revoked_at IS NULL`,
		digest, userID)
	if err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

func (r *Repo) RevokeUserTokens(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID)
	if err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	return nil
}
