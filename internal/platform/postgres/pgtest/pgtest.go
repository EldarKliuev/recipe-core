// Package pgtest gives integration tests a Postgres with the production schema.
// It is imported only from files tagged `integration`; the application never
// links it.
//
// Every Pool call drops and recreates the public schema, so packages that share
// TEST_DATABASE_URL must not run concurrently: `make test-integration` passes
// `-p 1` for that reason.
package pgtest

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Pool returns a pool against TEST_DATABASE_URL if set, otherwise it starts a
// throwaway Postgres container. Reusing an existing database is much faster in a
// tight edit-test loop. Without either, the test is skipped, and
// `make test-integration` turns that skip into a failure.
func Pool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	var terminate func()

	if dsn == "" {
		container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
			tcpostgres.WithDatabase("recipes_test"),
			tcpostgres.WithUsername("recipes"),
			tcpostgres.WithPassword("recipes"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second)),
		)
		if err != nil {
			t.Skipf("could not start Postgres (is Docker running?): %v", err)
		}
		dsn, err = container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("connection string: %v", err)
		}
		terminate = func() { _ = testcontainers.TerminateContainer(container) }
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("Postgres not reachable at %s: %v", dsn, err)
	}

	applyMigrations(t, pool)
	return pool, func() {
		pool.Close()
		if terminate != nil {
			terminate()
		}
	}
}

// User inserts a bare user row and returns its id, for tests of tables that
// reference users. The prefs row registration would add is left out; tests that
// need it insert it themselves.
func User(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		id, id.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// applyMigrations runs the embedded SQL directly. goose is skipped here so the
// test does not need a database/sql handle alongside the pgx pool; the point is a
// schema identical to production, not exercising goose itself.
func applyMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// Start from a clean schema so repeated runs against a reused database are
	// independent.
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)

	for _, name := range names {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, upSection(string(body))); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}

// upSection extracts everything between the goose Up and Down markers.
func upSection(body string) string {
	const upMarker = "-- +goose Up"
	const downMarker = "-- +goose Down"
	start := strings.Index(body, upMarker)
	if start < 0 {
		return body
	}
	start += len(upMarker)
	if end := strings.Index(body[start:], downMarker); end >= 0 {
		return body[start : start+end]
	}
	return body[start:]
}
