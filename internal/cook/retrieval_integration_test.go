//go:build integration

// Integration tests for the set-coverage query. They run against a real Postgres,
// because the behaviour under test *is* the SQL: coverage counting, the allergen
// NOT EXISTS, and the ranking order. A mock would only assert that the string is
// unchanged.
//
//	make test-integration            # uses the compose Postgres
//	go test -tags=integration ./...  # starts a throwaway container
package cook

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/platform/postgres/pgtest"
)

func TestSetCoverageRanksByMissingThenTotal(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)

	repo := NewRetrievalRepo(pool)
	rows, err := repo.Coverage(context.Background(), Query{
		UserID:        seed.userID,
		MaxMissing:    3,
		Limit:         20,
		UseSoonBefore: time.Now().Add(72 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("Coverage() returned nothing; the fridge should match several recipes")
	}

	// "Chicken and rice" is fully covered, so it must come first.
	if rows[0].RecipeID != seed.chickenRice {
		t.Errorf("first hit = %d, want the fully covered recipe %d", rows[0].RecipeID, seed.chickenRice)
	}
	if rows[0].MissingCount != 0 {
		t.Errorf("missing_count = %d for the fully covered recipe, want 0", rows[0].MissingCount)
	}

	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		if cur.MissingCount < prev.MissingCount {
			t.Fatalf("row %d has missing_count %d after %d; ordering is wrong",
				i, cur.MissingCount, prev.MissingCount)
		}
		if cur.MissingCount == prev.MissingCount && cur.TotalCount < prev.TotalCount {
			t.Fatalf("row %d breaks the total_count tie-break (%d after %d)",
				i, cur.TotalCount, prev.TotalCount)
		}
	}
}

// Staples must never be counted as missing, otherwise every recipe looks
// unreachable because nobody lists salt in their fridge.
func TestSetCoverageTreatsStaplesAsAvailable(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)

	repo := NewRetrievalRepo(pool)
	rows, err := repo.Coverage(context.Background(), Query{
		UserID: seed.userID, MaxMissing: 0, Limit: 20,
		UseSoonBefore: time.Now(),
	})
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}

	var found bool
	for _, row := range rows {
		if row.RecipeID == seed.chickenRice {
			found = true
			// The recipe lists salt, which is a staple and not in the fridge.
			if row.TotalCount != 3 {
				t.Errorf("total_count = %d, want 3 ingredient lines", row.TotalCount)
			}
		}
	}
	if !found {
		t.Error("the salt-using recipe was excluded at max_missing=0, so staples were counted as missing")
	}
}

func TestSetCoverageExcludesDeniedAllergens(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)

	repo := NewRetrievalRepo(pool)
	rows, err := repo.Coverage(context.Background(), Query{
		UserID: seed.userID, Allergens: []string{"dairy"},
		MaxMissing: 5, Limit: 20, UseSoonBefore: time.Now(),
	})
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}

	for _, row := range rows {
		if row.RecipeID == seed.creamyPasta {
			t.Fatalf("recipe %d contains a dairy ingredient but was returned for a dairy-allergic user",
				seed.creamyPasta)
		}
	}
}

func TestSetCoverageExcludesDislikedIngredients(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)

	repo := NewRetrievalRepo(pool)
	rows, err := repo.Coverage(context.Background(), Query{
		UserID: seed.userID, DislikedIDs: []int64{seed.chickenID},
		MaxMissing: 5, Limit: 20, UseSoonBefore: time.Now(),
	})
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}
	for _, row := range rows {
		if row.RecipeID == seed.chickenRice {
			t.Fatal("a recipe using a disliked ingredient was returned")
		}
	}
}

func TestSetCoverageRespectsMaxMissingAndComplexityFilters(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	repo := NewRetrievalRepo(pool)

	strict, err := repo.Coverage(context.Background(), Query{
		UserID: seed.userID, MaxMissing: 0, Limit: 20, UseSoonBefore: time.Now(),
	})
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}
	for _, row := range strict {
		if row.MissingCount != 0 {
			t.Errorf("max_missing=0 returned a recipe with missing_count %d", row.MissingCount)
		}
	}

	simple, err := repo.Coverage(context.Background(), Query{
		UserID: seed.userID, MaxMissing: 5, MaxSteps: 2, Limit: 20, UseSoonBefore: time.Now(),
	})
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}
	for _, row := range simple {
		if row.RecipeID == seed.longRecipe {
			t.Error("max_steps=2 returned the nine-step recipe")
		}
	}
}

func TestIsRetrievableForGuardsAdaptation(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	repo := NewRetrievalRepo(pool)

	q := Query{UserID: seed.userID, MaxMissing: 0, UseSoonBefore: time.Now()}
	ok, err := repo.IsRetrievableFor(context.Background(), q, seed.chickenRice)
	if err != nil {
		t.Fatalf("IsRetrievableFor() error = %v", err)
	}
	if !ok {
		t.Error("the fully covered recipe is reported as not retrievable")
	}

	// The same recipe must be refused once its allergen is denied.
	q.Allergens = []string{"dairy"}
	ok, err = repo.IsRetrievableFor(context.Background(), q, seed.creamyPasta)
	if err != nil {
		t.Fatalf("IsRetrievableFor() error = %v", err)
	}
	if ok {
		t.Error("a dairy recipe is reported as retrievable for a dairy-allergic user")
	}
}

// Adaptation checks a chosen recipe at max_missing=10, so anything search could
// have shown must pass, while allergens and dislikes still refuse it.
func TestIsRetrievableForAtTheWidestMaxMissing(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	repo := NewRetrievalRepo(pool)
	ctx := context.Background()
	q := Query{UserID: seed.userID, MaxMissing: maxMaxMissing, UseSoonBefore: time.Now()}

	cases := []struct {
		name     string
		recipe   int64
		mutate   func(*Query)
		expected bool
	}{
		{"five missing", seed.fiveShort, nil, true},
		{"eleven missing", seed.elevenShort, nil, false},
		{"five missing with a disliked ingredient", seed.fiveShort,
			func(q *Query) { q.DislikedIDs = []int64{seed.basilID} }, false},
		{"dairy recipe for a dairy-allergic user", seed.creamyPasta,
			func(q *Query) { q.Allergens = []string{"dairy"} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cq := q
			if tc.mutate != nil {
				tc.mutate(&cq)
			}
			ok, err := repo.IsRetrievableFor(ctx, cq, tc.recipe)
			if err != nil {
				t.Fatalf("IsRetrievableFor() error = %v", err)
			}
			if ok != tc.expected {
				t.Errorf("IsRetrievableFor() = %v, want %v", ok, tc.expected)
			}
		})
	}
}

func TestHaveIngredientIDsIncludesStaples(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)

	ids, err := NewRetrievalRepo(pool).HaveIngredientIDs(context.Background(), seed.userID)
	if err != nil {
		t.Fatalf("HaveIngredientIDs() error = %v", err)
	}
	var hasSalt, hasChicken bool
	for _, id := range ids {
		hasSalt = hasSalt || id == seed.saltID
		hasChicken = hasChicken || id == seed.chickenID
	}
	if !hasSalt {
		t.Error("staples are missing from the have set")
	}
	if !hasChicken {
		t.Error("fridge contents are missing from the have set")
	}
}

// --- fixtures ---

type corpusSeed struct {
	userID      uuid.UUID
	chickenRice int64
	creamyPasta int64
	longRecipe  int64
	fiveShort   int64
	elevenShort int64
	chickenID   int64
	basilID     int64
	saltID      int64
}

// testPool keeps the name every cook integration test already uses; the
// schema setup itself lives in pgtest, shared with the other packages.
func testPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	return pgtest.Pool(t)
}

func seedCorpus(t *testing.T, pool *pgxpool.Pool) corpusSeed {
	t.Helper()
	ctx := context.Background()
	seed := corpusSeed{userID: uuid.New()}

	ingredients := map[string]*int64{}
	for _, name := range []string{"chicken breast", "rice", "salt", "heavy cream", "pasta",
		"tomato", "onion", "garlic", "basil", "parmesan", "saffron", "truffle",
		"lemon", "thyme", "paprika", "cumin", "ginger", "fennel"} {
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO ingredients_catalog (canonical_name, category) VALUES ($1, 'test')
			 ON CONFLICT (lower(canonical_name)) DO UPDATE SET category = 'test' RETURNING id`,
			name).Scan(&id)
		if err != nil {
			t.Fatalf("insert ingredient %q: %v", name, err)
		}
		local := id
		ingredients[name] = &local
	}
	id := func(name string) int64 { return *ingredients[name] }

	seed.chickenID = id("chicken breast")
	seed.basilID = id("basil")
	seed.saltID = id("salt")

	// Salt is a staple, so it must not count as missing anywhere.
	if _, err := pool.Exec(ctx,
		`INSERT INTO pantry_staples (ingredient_id) VALUES ($1) ON CONFLICT DO NOTHING`,
		seed.saltID); err != nil {
		t.Fatalf("insert staple: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ingredient_allergens (ingredient_id, allergen) VALUES ($1, 'dairy'), ($2, 'dairy')
		 ON CONFLICT DO NOTHING`, id("heavy cream"), id("parmesan")); err != nil {
		t.Fatalf("insert allergens: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		seed.userID, fmt.Sprintf("cook-%s@example.com", seed.userID)); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_prefs (user_id) VALUES ($1)`, seed.userID); err != nil {
		t.Fatalf("insert prefs: %v", err)
	}

	// The fridge: enough for chicken and rice, nearly enough for pasta.
	for _, name := range []string{"chicken breast", "rice", "pasta", "tomato", "onion", "garlic"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_ingredients (user_id, ingredient_id) VALUES ($1, $2)`,
			seed.userID, id(name)); err != nil {
			t.Fatalf("insert fridge item %q: %v", name, err)
		}
	}

	insertRecipe := func(title string, steps int, names ...string) int64 {
		// Real step text, not an empty array: these are the verbatim corpus
		// directions sent to the model for paraphrasing, so anything reading the
		// steps column needs something to read.
		directions := make([]string, steps)
		for i := range directions {
			directions[i] = fmt.Sprintf("Corpus step %d of %s.", i+1, title)
		}
		body, err := json.Marshal(directions)
		if err != nil {
			t.Fatalf("marshal directions: %v", err)
		}
		var recipeID int64
		err = pool.QueryRow(ctx, `
INSERT INTO recipes (external_id, title, category, steps, num_ingredients, num_steps, source)
VALUES ($1, $2, 'test', $3::jsonb, $4, $5, 'test') RETURNING id`,
			title, title, body, len(names), steps).Scan(&recipeID)
		if err != nil {
			t.Fatalf("insert recipe %q: %v", title, err)
		}
		for _, name := range names {
			if _, err := pool.Exec(ctx, `
INSERT INTO recipe_ingredients (recipe_id, ingredient_id, raw_text) VALUES ($1, $2, $3)`,
				recipeID, id(name), name); err != nil {
				t.Fatalf("link %q to %q: %v", name, title, err)
			}
		}
		return recipeID
	}

	// Fully covered (salt is a staple).
	seed.chickenRice = insertRecipe("Chicken and rice", 3, "chicken breast", "rice", "salt")
	// Contains dairy, so it must vanish for a dairy-allergic user.
	seed.creamyPasta = insertRecipe("Creamy pasta", 4, "pasta", "heavy cream", "parmesan")
	// Two ingredients short: exercises the max_missing threshold.
	insertRecipe("Tomato pasta", 5, "pasta", "tomato", "basil", "saffron")
	// Long recipe for the complexity filter.
	seed.longRecipe = insertRecipe("Elaborate risotto", 9, "rice", "onion", "garlic", "truffle")
	// Five and eleven ingredients short: adaptation accepts a chosen recipe up to
	// the widest max_missing search allows (10), and no further.
	seed.fiveShort = insertRecipe("Herb pasta", 6, "pasta", "basil", "saffron", "truffle",
		"lemon", "thyme")
	seed.elevenShort = insertRecipe("Spice feast", 7, "rice", "basil", "saffron", "truffle",
		"lemon", "thyme", "paprika", "cumin", "ginger", "fennel", "heavy cream", "parmesan")

	return seed
}
