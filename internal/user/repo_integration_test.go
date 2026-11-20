//go:build integration

// Prefs hold the allergen deny-list, so one user's save leaking into another's
// row would either hide recipes or, worse, drop someone's allergy.
package user

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/recipe-ai/core-api/internal/platform/postgres/pgtest"
)

func TestSavePrefsChangesOnlyTheCallersRow(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner, other := pgtest.User(t, pool), pgtest.User(t, pool)

	if err := repo.SavePrefs(ctx, owner, Prefs{Allergens: []string{"peanuts"}}); err != nil {
		t.Fatalf("SavePrefs(owner) error = %v", err)
	}
	if err := repo.SavePrefs(ctx, other, Prefs{Allergens: []string{"dairy"}}); err != nil {
		t.Fatalf("SavePrefs(other) error = %v", err)
	}

	got, err := repo.Prefs(ctx, owner)
	if err != nil {
		t.Fatalf("Prefs() error = %v", err)
	}
	if !slices.Equal(got.Allergens, []string{"peanuts"}) {
		t.Errorf("owner's allergens = %v, want [peanuts]", got.Allergens)
	}
}

func TestSavePrefsStoresNilListsAsEmpty(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepo(pool)
	owner := pgtest.User(t, pool)

	if err := repo.SavePrefs(ctx, owner, Prefs{}); err != nil {
		t.Fatalf("SavePrefs() error = %v", err)
	}
	got, err := repo.Prefs(ctx, owner)
	if err != nil {
		t.Fatalf("Prefs() error = %v", err)
	}
	if got.Allergens == nil || len(got.Allergens) != 0 || got.DislikedIngredientIDs == nil {
		t.Errorf("Prefs() = %+v, want empty non-nil lists", got)
	}
}

func TestPrefsOfAUserWithoutARowIsNotFound(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	repo := NewRepo(pool)
	// pgtest.User does not create the prefs row that registration would.
	owner := pgtest.User(t, pool)

	_, err := repo.Prefs(context.Background(), owner)
	if !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("Prefs() error = %v, want ErrNotFound", err)
	}
}
