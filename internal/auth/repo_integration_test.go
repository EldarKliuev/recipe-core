//go:build integration

// service_test.go covers token rotation against a fake repo. These tests pin the
// SQL that the fake stands in for: the single-use UPDATE ... RETURNING, expiry,
// per-user revocation and the case-insensitive email index.
package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/recipe-ai/core-api/internal/platform/postgres/pgtest"
)

func TestRefreshTokenIsConsumedOnlyOnce(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner := pgtest.User(t, pool)
	digest := []byte("digest-1")

	if err := repo.StoreRefreshToken(ctx, digest, owner, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("StoreRefreshToken() error = %v", err)
	}
	got, err := repo.ConsumeRefreshToken(ctx, digest)
	if err != nil {
		t.Fatalf("first ConsumeRefreshToken() error = %v", err)
	}
	if got != owner {
		t.Errorf("token owner = %s, want %s", got, owner)
	}

	if _, err := repo.ConsumeRefreshToken(ctx, digest); !errors.Is(err, httpx.ErrUnauthorized) {
		t.Fatalf("replayed ConsumeRefreshToken() error = %v, want ErrUnauthorized", err)
	}
}

func TestExpiredOrUnknownRefreshTokenIsRejected(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner := pgtest.User(t, pool)
	expired := []byte("expired")

	if err := repo.StoreRefreshToken(ctx, expired, owner, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("StoreRefreshToken() error = %v", err)
	}
	if _, err := repo.ConsumeRefreshToken(ctx, expired); !errors.Is(err, httpx.ErrUnauthorized) {
		t.Errorf("expired token error = %v, want ErrUnauthorized", err)
	}
	if _, err := repo.ConsumeRefreshToken(ctx, []byte("never-issued")); !errors.Is(err, httpx.ErrUnauthorized) {
		t.Errorf("unknown token error = %v, want ErrUnauthorized", err)
	}
}

func TestRevokeUserTokensLeavesOtherUsersTokensLive(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner, other := pgtest.User(t, pool), pgtest.User(t, pool)
	expires := time.Now().Add(time.Hour)

	if err := repo.StoreRefreshToken(ctx, []byte("owner"), owner, expires); err != nil {
		t.Fatalf("StoreRefreshToken(owner) error = %v", err)
	}
	if err := repo.StoreRefreshToken(ctx, []byte("other"), other, expires); err != nil {
		t.Fatalf("StoreRefreshToken(other) error = %v", err)
	}

	if err := repo.RevokeUserTokens(ctx, owner); err != nil {
		t.Fatalf("RevokeUserTokens() error = %v", err)
	}
	if _, err := repo.ConsumeRefreshToken(ctx, []byte("owner")); !errors.Is(err, httpx.ErrUnauthorized) {
		t.Errorf("revoked token error = %v, want ErrUnauthorized", err)
	}
	if got, err := repo.ConsumeRefreshToken(ctx, []byte("other")); err != nil || got != other {
		t.Errorf("other user's token = (%s, %v), want it still usable", got, err)
	}
}

func TestCreateUserRejectsAnEmailThatDiffersOnlyInCase(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)

	first := User{ID: uuid.New(), Email: "Cook@Example.test", PasswordHash: "h"}
	if err := repo.CreateUser(ctx, first); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	err := repo.CreateUser(ctx, User{ID: uuid.New(), Email: "cook@example.test", PasswordHash: "h"})
	if !errors.Is(err, httpx.ErrConflict) {
		t.Fatalf("second CreateUser() error = %v, want ErrConflict", err)
	}

	got, err := repo.UserByEmail(ctx, "COOK@example.test")
	if err != nil {
		t.Fatalf("UserByEmail() error = %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("UserByEmail() id = %s, want %s", got.ID, first.ID)
	}
	// Registration also creates the prefs row in the same transaction.
	var prefs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_prefs WHERE user_id = $1`, first.ID).Scan(&prefs); err != nil {
		t.Fatalf("count prefs: %v", err)
	}
	if prefs != 1 {
		t.Errorf("prefs rows = %d, want 1", prefs)
	}
}
