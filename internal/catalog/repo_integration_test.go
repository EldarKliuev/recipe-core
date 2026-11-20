//go:build integration

// ResolveNames is what the validator's allergen check stands on: a model name
// that resolves to the wrong entry, or to nothing, skips the deny-list. The
// singular-form cases live in internal/cook/resolve_names_integration_test.go;
// these cover the remaining ranks: exact, synonym, fuzzy and its threshold.
package catalog

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/platform/postgres/pgtest"
)

// ingredient inserts a catalog entry with optional synonyms and returns its id.
func ingredient(t *testing.T, pool *pgxpool.Pool, name string, synonyms ...string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO ingredients_catalog (canonical_name, category) VALUES ($1, 'test') RETURNING id`,
		name).Scan(&id); err != nil {
		t.Fatalf("insert ingredient %q: %v", name, err)
	}
	for _, s := range synonyms {
		if _, err := pool.Exec(ctx,
			`INSERT INTO ingredient_synonyms (ingredient_id, synonym) VALUES ($1, $2)`, id, s); err != nil {
			t.Fatalf("insert synonym %q: %v", s, err)
		}
	}
	return id
}

func TestResolveNamesByExactNameSynonymAndFuzzyMatch(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	breast := ingredient(t, pool, "chicken breast")
	cilantro := ingredient(t, pool, "cilantro", "coriander leaf")
	repo := NewRepo(pool)

	cases := []struct {
		input     string
		wantID    int64
		wantExact bool
	}{
		{"Chicken Breast", breast, true},
		{"coriander leaf", cilantro, true},
		// A typo is still found, but flagged so the caller can tell it apart.
		{"chiken breast", breast, false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			resolved, unresolved, err := repo.ResolveNames(context.Background(), []string{tc.input})
			if err != nil {
				t.Fatalf("ResolveNames() error = %v", err)
			}
			if len(resolved) != 1 {
				t.Fatalf("resolved = %v, unresolved = %v, want one match", resolved, unresolved)
			}
			got := resolved[0]
			if got.Ingredient.ID != tc.wantID || got.Exact != tc.wantExact || got.Input != tc.input {
				t.Errorf("got id %d exact %v input %q, want id %d exact %v input %q",
					got.Ingredient.ID, got.Exact, got.Input, tc.wantID, tc.wantExact, tc.input)
			}
		})
	}
}

func TestResolveNamesLeavesWeakFuzzyMatchesUnresolved(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ingredient(t, pool, "beer")
	repo := NewRepo(pool)

	// "beef" shares most trigrams with "beer"; mapping one onto the other would
	// corrupt a fridge, so it must stay below fuzzyThreshold.
	resolved, unresolved, err := repo.ResolveNames(context.Background(), []string{"beef", "   "})
	if err != nil {
		t.Fatalf("ResolveNames() error = %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("resolved = %+v, want nothing", resolved)
	}
	if len(unresolved) != 2 || unresolved[0] != "beef" {
		t.Errorf("unresolved = %q, want both inputs back as written", unresolved)
	}
}

func TestSearchRanksPrefixThenSubstringThenSynonymThenFuzzy(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	prefix := ingredient(t, pool, "chickpea")
	substring := ingredient(t, pool, "roast chicken")
	synonym := ingredient(t, pool, "garbanzo", "chick pea flour")
	// No "chic" substring anywhere, only trigram overlap with the query.
	fuzzy := ingredient(t, pool, "chikn")
	ingredient(t, pool, "tomato")
	repo := NewRepo(pool)

	got, err := repo.Search(context.Background(), "  CHIC ", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	ids := make([]int64, len(got))
	for i, ing := range got {
		ids[i] = ing.ID
	}
	want := map[int64]int{prefix: 1, substring: 2, synonym: 3, fuzzy: 4}
	if len(ids) != len(want) {
		t.Fatalf("Search() ids = %v, want exactly %v", ids, want)
	}
	last := 0
	for _, id := range ids {
		rank, ok := want[id]
		if !ok {
			t.Fatalf("Search() returned unexpected id %d in %v", id, ids)
		}
		if rank < last {
			t.Errorf("Search() order %v puts rank %d after rank %d", ids, rank, last)
		}
		last = rank
	}
}

func TestSearchFindsNothingForAnUnrelatedQuery(t *testing.T) {
	pool, cleanup := pgtest.Pool(t)
	defer cleanup()
	ingredient(t, pool, "chickpea")
	repo := NewRepo(pool)

	got, err := repo.Search(context.Background(), "zzzz", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Search() = %+v, want nothing", got)
	}
}
