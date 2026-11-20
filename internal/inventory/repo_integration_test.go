//go:build integration

// The fridge is per user: every query carries user_id, and an ingredient id from
// the path grants nothing by itself (CLAUDE.md §5.5). These tests pin that on a
// real Postgres, where the WHERE clause is what is being tested.
package inventory

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/recipe-ai/core-api/internal/platform/postgres/pgtest"
)

func ingredient(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO ingredients_catalog (canonical_name, category) VALUES ($1, 'test') RETURNING id`,
		name).Scan(&id); err != nil {
		t.Fatalf("insert ingredient %q: %v", name, err)
	}
	return id
}

func ptr[T any](v T) *T { return &v }

func TestAnotherUsersFridgeIsNotReadable(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner, other := pgtest.User(t, pool), pgtest.User(t, pool)
	milk := ingredient(t, pool, "milk")

	if err := repo.Upsert(ctx, owner, milk, ptr(1.0), ptr("l"), nil); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	items, err := repo.List(ctx, other)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 0 {
		t.Errorf("other user's List() = %+v, want empty", items)
	}
	ids, err := repo.IngredientIDs(ctx, other)
	if err != nil {
		t.Fatalf("IngredientIDs() error = %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("other user's IngredientIDs() = %v, want empty", ids)
	}

	// The owner still sees their own item, so the empty lists above are not just
	// a broken query.
	items, err = repo.List(ctx, owner)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].IngredientID != milk {
		t.Errorf("owner's List() = %+v, want only milk", items)
	}
}

func TestAnotherUsersFridgeItemCannotBeDeleted(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner, other := pgtest.User(t, pool), pgtest.User(t, pool)
	milk := ingredient(t, pool, "milk")

	if err := repo.Upsert(ctx, owner, milk, nil, nil, nil); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	err := repo.Delete(ctx, other, milk)
	if !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("Delete() by another user error = %v, want ErrNotFound (404)", err)
	}
	ids, err := repo.IngredientIDs(ctx, owner)
	if err != nil {
		t.Fatalf("IngredientIDs() error = %v", err)
	}
	if len(ids) != 1 || ids[0] != milk {
		t.Errorf("owner's fridge after foreign delete = %v, want [%d]", ids, milk)
	}

	if err := repo.Delete(ctx, owner, milk); err != nil {
		t.Fatalf("Delete() by owner error = %v", err)
	}
}

func TestUpsertBySomeoneElseDoesNotChangeTheOwnersItem(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner, other := pgtest.User(t, pool), pgtest.User(t, pool)
	milk := ingredient(t, pool, "milk")

	if err := repo.Upsert(ctx, owner, milk, ptr(1.0), ptr("l"), nil); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	// The conflict target is (user_id, ingredient_id), so this must create the
	// other user's row rather than update the owner's.
	if err := repo.Upsert(ctx, other, milk, ptr(5.0), ptr("ml"), nil); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	items, err := repo.List(ctx, owner)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].Qty == nil || *items[0].Qty != 1.0 || *items[0].Unit != "l" {
		t.Errorf("owner's item = %+v, want qty 1 l", items)
	}
}

func TestUpsertRejectsAnIngredientOutsideTheCatalog(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	repo := NewRepo(pool)
	owner := pgtest.User(t, pool)

	err := repo.Upsert(context.Background(), owner, 999_999, nil, nil, nil)
	if !errors.Is(err, httpx.ErrInvalidInput) {
		t.Fatalf("Upsert() error = %v, want ErrInvalidInput (400)", err)
	}
}
