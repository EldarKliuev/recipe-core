//go:build integration

// service_test.go covers token rotation against a fake repo. These tests pin the
// SQL that the fake stands in for: the single-use UPDATE ... RETURNING, expiry,
// per-user revocation and the case-insensitive email index.
package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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

// Logout revokes by token and owner together: the caller's token stops working,
// a foreign token is left alone, and repeating the logout is not an error.
func TestRevokeRefreshTokenOnlyRevokesTheOwnersToken(t *testing.T) {
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

	if err := repo.RevokeRefreshToken(ctx, []byte("other"), owner); err != nil {
		t.Fatalf("RevokeRefreshToken(foreign) error = %v", err)
	}
	for range 2 {
		if err := repo.RevokeRefreshToken(ctx, []byte("owner"), owner); err != nil {
			t.Fatalf("RevokeRefreshToken(own) error = %v", err)
		}
	}

	if _, err := repo.ConsumeRefreshToken(ctx, []byte("owner")); !errors.Is(err, httpx.ErrUnauthorized) {
		t.Errorf("revoked token error = %v, want ErrUnauthorized", err)
	}
	if got, err := repo.ConsumeRefreshToken(ctx, []byte("other")); err != nil || got != other {
		t.Errorf("foreign token = (%s, %v), want it still usable", got, err)
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

// userTables are every table holding per-user rows. A new one belongs here, or
// account deletion is no longer checked for it.
var userTables = []string{"user_prefs", "user_ingredients", "refresh_tokens", "adapted_recipes", "generation_runs"}

// seedUserRows gives userID one row in every table of userTables. withAccount
// skips user_prefs and refresh_tokens, which registration already created.
func seedUserRows(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, recipeID, ingredientID int64, withAccount bool) {
	t.Helper()
	type row struct {
		sql  string
		args []any
	}
	rows := []row{
		{`INSERT INTO user_ingredients (user_id, ingredient_id) VALUES ($1, $2)`, []any{userID, ingredientID}},
		{`INSERT INTO adapted_recipes (id, user_id, base_recipe_id, result) VALUES (gen_random_uuid(), $1, $2, '{}')`, []any{userID, recipeID}},
		{`INSERT INTO generation_runs (id, user_id, job_id, status) VALUES (gen_random_uuid(), $1, gen_random_uuid(), 'success')`, []any{userID}},
	}
	if !withAccount {
		rows = append(rows,
			row{`INSERT INTO user_prefs (user_id) VALUES ($1)`, []any{userID}},
			row{`INSERT INTO refresh_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, now() + interval '1 hour')`,
				[]any{[]byte("seed-" + userID.String()), userID}},
		)
	}
	for _, r := range rows {
		if _, err := pool.Exec(context.Background(), r.sql, r.args...); err != nil {
			t.Fatalf("seed %q: %v", r.sql, err)
		}
	}
}

func userRowCounts(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range userTables {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM `+table+` WHERE user_id = $1`, userID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}
	return counts
}

// TestDeleteAccountRemovesOnlyTheCallersData runs DELETE /me through the real
// middleware and repo: every row of the caller is gone, their refresh token and
// old access token stop working, and the other user is untouched.
func TestDeleteAccountRemovesOnlyTheCallersData(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	issuer := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	svc := NewService(repo, issuer)

	var recipeID, ingredientID int64
	if err := pool.QueryRow(ctx, `INSERT INTO recipes (title) VALUES ('toast') RETURNING id`).Scan(&recipeID); err != nil {
		t.Fatalf("insert recipe: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ingredients_catalog (canonical_name, category) VALUES ('delete-test bread', 'test') RETURNING id`,
	).Scan(&ingredientID); err != nil {
		t.Fatalf("insert ingredient: %v", err)
	}

	tokens, err := svc.Register(ctx, "owner@example.test", goodPassword)
	if err != nil {
		t.Fatalf("Register(owner) error = %v", err)
	}
	owner, err := repo.UserByEmail(ctx, "owner@example.test")
	if err != nil {
		t.Fatalf("UserByEmail(owner) error = %v", err)
	}
	other := pgtest.User(t, pool)
	// Registration already made the owner's prefs and refresh token rows.
	seedUserRows(t, pool, owner.ID, recipeID, ingredientID, true)
	seedUserRows(t, pool, other, recipeID, ingredientID, false)
	for table, n := range userRowCounts(t, pool, owner.ID) {
		if n == 0 {
			t.Fatalf("owner has no rows in %s before deletion; the test would prove nothing", table)
		}
	}

	r := chi.NewRouter()
	r.Use(Middleware(issuer, repo))
	r.Delete("/api/v1/me", NewHandler(svc).DeleteAccount)
	r.Get("/api/v1/me/prefs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	call := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call(http.MethodDelete, "/api/v1/me", `{"password":"not the password"}`); code != http.StatusForbidden {
		t.Fatalf("DELETE /me with a wrong password = %d, want 403", code)
	}
	if code := call(http.MethodDelete, "/api/v1/me", `{"password":"`+goodPassword+`"}`); code != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d, want 204", code)
	}

	for table, n := range userRowCounts(t, pool, owner.ID) {
		if n != 0 {
			t.Errorf("%s still has %d rows of the deleted user", table, n)
		}
	}
	for table, n := range userRowCounts(t, pool, other) {
		if n != 1 {
			t.Errorf("%s has %d rows of the other user, want 1: deletion touched someone else", table, n)
		}
	}
	if _, err := svc.Refresh(ctx, tokens.RefreshToken); !errors.Is(err, httpx.ErrUnauthorized) {
		t.Errorf("refresh after deletion error = %v, want ErrUnauthorized", err)
	}
	if code := call(http.MethodGet, "/api/v1/me/prefs", ""); code != http.StatusUnauthorized {
		t.Errorf("request with the old access token = %d, want 401", code)
	}
}
