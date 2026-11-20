//go:build integration

// History is per user: an adapted recipe id from the path grants nothing by
// itself, and a foreign id answers 404 so ids cannot be probed (CLAUDE.md §5.5).
package recipehistory

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/recipe-ai/core-api/internal/platform/postgres/pgtest"
)

// savedEntry stores one adapted recipe for userID on top of a fresh corpus row.
func savedEntry(t *testing.T, pool *pgxpool.Pool, repo *Repo, userID uuid.UUID) Entry {
	t.Helper()
	ctx := context.Background()
	var recipeID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO recipes (title) VALUES ('Pancakes') RETURNING id`).Scan(&recipeID); err != nil {
		t.Fatalf("insert recipe: %v", err)
	}
	entry := Entry{
		ID:           uuid.New(),
		UserID:       userID,
		BaseRecipeID: recipeID,
		Result:       []byte(`{"title":"Pancakes"}`),
		Adapted:      true,
	}
	if err := repo.SaveAdapted(ctx, entry); err != nil {
		t.Fatalf("SaveAdapted() error = %v", err)
	}
	return entry
}

func TestAnotherUsersHistoryIsNotReadable(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner, other := pgtest.User(t, pool), pgtest.User(t, pool)
	entry := savedEntry(t, pool, repo, owner)

	items, err := repo.List(ctx, other, 20, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 0 {
		t.Errorf("other user's List() = %+v, want empty", items)
	}

	items, err = repo.List(ctx, owner, 20, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != entry.ID || items[0].BaseTitle != "Pancakes" {
		t.Errorf("owner's List() = %+v, want the saved entry with its corpus title", items)
	}
}

func TestAnotherUsersHistoryCannotBeRated(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner, other := pgtest.User(t, pool), pgtest.User(t, pool)
	entry := savedEntry(t, pool, repo, owner)

	err := repo.SetRating(ctx, other, entry.ID, 1)
	if !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("SetRating() by another user error = %v, want ErrNotFound (404)", err)
	}
	items, err := repo.List(ctx, owner, 20, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].Rating != nil {
		t.Fatalf("owner's entry after foreign rating = %+v, want no rating", items)
	}

	if err := repo.SetRating(ctx, owner, entry.ID, 5); err != nil {
		t.Fatalf("SetRating() by owner error = %v", err)
	}
	items, err = repo.List(ctx, owner, 20, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if items[0].Rating == nil || *items[0].Rating != 5 {
		t.Errorf("owner's rating = %v, want 5", items[0].Rating)
	}
}

func TestRatingAnUnknownEntryIsNotFound(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	repo := NewRepo(pool)
	owner := pgtest.User(t, pool)

	err := repo.SetRating(context.Background(), owner, uuid.New(), 3)
	if !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("SetRating() error = %v, want ErrNotFound", err)
	}
}

func TestOwnHistoryEntryIsReadableByID(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	repo := NewRepo(pool)
	owner := pgtest.User(t, pool)
	entry := savedEntry(t, pool, repo, owner)

	got, err := repo.Get(context.Background(), owner, entry.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != entry.ID || got.BaseRecipeID != entry.BaseRecipeID ||
		got.BaseTitle != "Pancakes" || !got.Adapted || string(got.Result) != `{"title": "Pancakes"}` {
		t.Errorf("Get() = %+v, want the saved entry with its corpus title", got)
	}
}

func TestAnotherUsersHistoryEntryIsNotReadableByID(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	repo := NewRepo(pool)
	owner, other := pgtest.User(t, pool), pgtest.User(t, pool)
	entry := savedEntry(t, pool, repo, owner)

	got, err := repo.Get(context.Background(), other, entry.ID)
	if !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("Get() by another user = %+v, %v; want ErrNotFound (404)", got, err)
	}
}

func TestUnknownHistoryEntryIsNotFound(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	repo := NewRepo(pool)
	owner := pgtest.User(t, pool)
	savedEntry(t, pool, repo, owner)

	_, err := repo.Get(context.Background(), owner, uuid.New())
	if !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}
