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
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/catalog"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/recipe-ai/core-api/internal/platform/postgres/pgtest"
	"github.com/recipe-ai/core-api/internal/user"
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

// Search reads a copy of the links in recipes.ingredient_ids, while the tags stay
// in ingredient_allergens. A tag added after the recipe was loaded must apply at
// once, with no reload or recompute.
func TestSetCoverageExcludesAnAllergenTaggedAfterTheRecipeWasLoaded(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	ctx := context.Background()
	repo := NewRetrievalRepo(pool)

	var tomatoPasta int64
	if err := pool.QueryRow(ctx, `SELECT id FROM recipes WHERE title = 'Tomato pasta'`).
		Scan(&tomatoPasta); err != nil {
		t.Fatalf("find recipe: %v", err)
	}
	q := Query{UserID: seed.userID, Allergens: []string{"nightshade"},
		MaxMissing: 5, Limit: 20, UseSoonBefore: time.Now()}
	contains := func(rows []coverageRow, id int64) bool {
		for _, row := range rows {
			if row.RecipeID == id {
				return true
			}
		}
		return false
	}

	before, err := repo.Coverage(ctx, q)
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}
	if !contains(before, tomatoPasta) {
		t.Fatal("the tomato recipe is not returned before tomato is tagged, so the test proves nothing")
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO ingredient_allergens (ingredient_id, allergen)
SELECT id, 'nightshade' FROM ingredients_catalog WHERE canonical_name = 'tomato'`); err != nil {
		t.Fatalf("tag tomato: %v", err)
	}
	after, err := repo.Coverage(ctx, q)
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}
	if contains(after, tomatoPasta) {
		t.Fatal("a recipe with a newly tagged allergen was returned for a user who denies it")
	}
}

// recipes.ingredient_ids is what search reads, so every way of changing the links
// must reach it: a stale array would rank by the wrong ingredients and could let a
// recipe through the allergen overlap.
func TestRecipeIngredientIDsFollowTheLinks(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	ctx := context.Background()

	assertInStep := func(t *testing.T) {
		t.Helper()
		var stale int
		err := pool.QueryRow(ctx, `
SELECT count(*) FROM recipes r
WHERE r.ingredient_ids IS DISTINCT FROM coalesce(
    (SELECT array_agg(ri.ingredient_id::int ORDER BY ri.ingredient_id)
     FROM recipe_ingredients ri WHERE ri.recipe_id = r.id), '{}')`).Scan(&stale)
		if err != nil {
			t.Fatalf("compare arrays: %v", err)
		}
		if stale != 0 {
			t.Fatalf("%d recipes have ingredient_ids out of step with recipe_ingredients", stale)
		}
	}
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}

	t.Run("after the load", func(t *testing.T) {
		var ids []int64
		if err := pool.QueryRow(ctx, `SELECT ingredient_ids FROM recipes WHERE id = $1`,
			seed.chickenRice).Scan(&ids); err != nil {
			t.Fatalf("read array: %v", err)
		}
		if len(ids) != 3 {
			t.Fatalf("ingredient_ids = %v, want the three linked ingredients", ids)
		}
		assertInStep(t)
	})
	t.Run("delete", func(t *testing.T) {
		exec(t, `DELETE FROM recipe_ingredients WHERE recipe_id = $1 AND ingredient_id = $2`,
			seed.chickenRice, seed.saltID)
		assertInStep(t)
	})
	t.Run("multi-row insert", func(t *testing.T) {
		exec(t, `INSERT INTO recipe_ingredients (recipe_id, ingredient_id, raw_text)
VALUES ($1, $3, 'salt'), ($2, $3, 'salt')`, seed.chickenRice, seed.longRecipe, seed.saltID)
		assertInStep(t)
	})
	t.Run("update moves a link to another recipe", func(t *testing.T) {
		exec(t, `UPDATE recipe_ingredients SET recipe_id = $2 WHERE recipe_id = $1 AND ingredient_id = $3`,
			seed.chickenRice, seed.creamyPasta, seed.chickenID)
		assertInStep(t)
	})
	t.Run("recipe deleted with its links", func(t *testing.T) {
		exec(t, `DELETE FROM recipes WHERE id = $1`, seed.fiveShort)
		assertInStep(t)
	})
	t.Run("truncate", func(t *testing.T) {
		exec(t, `TRUNCATE recipe_ingredients`)
		assertInStep(t)
	})
}

// ingredient_ids is int4 because intarray is, while catalog ids are bigint. An id
// that does not fit must be refused when linked, never dropped from the array,
// and search must not fail when such an id is in a fridge or a dislike list.
func TestIngredientIDsOutsideInt4(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	ctx := context.Background()

	const bigID = int64(3_000_000_000)
	if _, err := pool.Exec(ctx,
		`INSERT INTO ingredients_catalog (id, canonical_name) VALUES ($1, 'oversized id')`, bigID); err != nil {
		t.Fatalf("insert catalog row: %v", err)
	}

	t.Run("linking it to a recipe is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO recipe_ingredients (recipe_id, ingredient_id, raw_text) VALUES ($1, $2, 'x')`,
			seed.chickenRice, bigID)
		if err == nil {
			t.Fatal("a link with an id outside int4 was stored, so ingredient_ids cannot hold it")
		}
	})

	t.Run("search ignores it in the fridge and the dislikes", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_ingredients (user_id, ingredient_id) VALUES ($1, $2)`,
			seed.userID, bigID); err != nil {
			t.Fatalf("insert fridge item: %v", err)
		}
		rows, err := NewRetrievalRepo(pool).Coverage(ctx, Query{
			UserID: seed.userID, DislikedIDs: []int64{bigID},
			MaxMissing: 0, Limit: 20, UseSoonBefore: time.Now(),
		})
		if err != nil {
			t.Fatalf("Coverage() error = %v", err)
		}
		if len(rows) == 0 || rows[0].RecipeID != seed.chickenRice {
			t.Fatalf("Coverage() = %v, want the fully covered recipe first", rows)
		}
	})
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

// Part of the corpus lists fewer ingredients than its steps use (TASK-40), so the
// deny-list also reads the ids ingest found in the directions. Coverage must not:
// a user without the allergy still sees the recipe, counted by its list alone.
func TestSetCoverageExcludesAnAllergenNamedOnlyInTheSteps(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	ctx := context.Background()
	repo := NewRetrievalRepo(pool)

	var truncated int64
	if err := pool.QueryRow(ctx, `
INSERT INTO recipes (external_id, title, category, steps, num_ingredients, num_steps, source,
                     step_ingredient_ids)
VALUES ('truncated', 'Truncated pasta', 'test',
        '["Boil the pasta.", "Toss with tomato and parmesan."]'::jsonb, 2, 2, 'test',
        ARRAY[$1, $2, $3]::int[])
RETURNING id`, seed.pastaID, seed.tomatoID, seed.parmesanID).Scan(&truncated); err != nil {
		t.Fatalf("insert recipe: %v", err)
	}
	for _, ingredient := range []int64{seed.pastaID, seed.tomatoID} {
		if _, err := pool.Exec(ctx, `
INSERT INTO recipe_ingredients (recipe_id, ingredient_id, raw_text) VALUES ($1, $2, 'x')`,
			truncated, ingredient); err != nil {
			t.Fatalf("link ingredient: %v", err)
		}
	}

	find := func(allergens []string) *coverageRow {
		t.Helper()
		rows, err := repo.Coverage(ctx, Query{UserID: seed.userID, Allergens: allergens,
			MaxMissing: 0, Limit: 50, UseSoonBefore: time.Now()})
		if err != nil {
			t.Fatalf("Coverage() error = %v", err)
		}
		for i := range rows {
			if rows[i].RecipeID == truncated {
				return &rows[i]
			}
		}
		return nil
	}

	row := find(nil)
	if row == nil {
		t.Fatal("without allergies the recipe is missing: step ids must not count as missing")
	}
	if row.TotalCount != 2 {
		t.Errorf("total_count = %d, want the 2 listed ingredients only", row.TotalCount)
	}
	if find([]string{"dairy"}) != nil {
		t.Fatal("a recipe whose steps name parmesan was returned for a dairy-allergic user")
	}

	q := Query{UserID: seed.userID, MaxMissing: maxMaxMissing, UseSoonBefore: time.Now()}
	if ok, err := repo.IsRetrievableFor(ctx, q, truncated); err != nil || !ok {
		t.Fatalf("IsRetrievableFor() without allergies = %v, %v; want true", ok, err)
	}
	q.Allergens = []string{"dairy"}
	if ok, err := repo.IsRetrievableFor(ctx, q, truncated); err != nil || ok {
		t.Fatalf("IsRetrievableFor() for a dairy-allergic user = %v, %v; want false", ok, err)
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
	pastaID     int64
	tomatoID    int64
	parmesanID  int64
}

// recipeService is the search path over real repositories; the adaptation
// dependencies are not needed for it.
func recipeService(pool *pgxpool.Pool) *Service {
	return NewService(NewRetrievalRepo(pool), catalog.NewRepo(pool), user.NewRepo(pool), nil, nil, nil)
}

// The card and the search must agree on every field for the same recipe, which
// is the point of building both from one query.
func TestRecipeByIDMatchesTheSearchResult(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	svc := recipeService(pool)
	ctx := context.Background()

	search, err := svc.Matches(ctx, seed.userID, MatchesRequest{})
	if err != nil {
		t.Fatalf("Matches() error = %v", err)
	}
	for _, want := range []int64{seed.chickenRice, seed.creamyPasta} {
		var fromSearch *Match
		for i := range search.Items {
			if search.Items[i].RecipeID == want {
				fromSearch = &search.Items[i]
			}
		}
		if fromSearch == nil {
			t.Fatalf("recipe %d is not in the search results; the seed changed", want)
		}
		got, err := svc.Recipe(ctx, seed.userID, want)
		if err != nil {
			t.Fatalf("Recipe(%d) error = %v", want, err)
		}
		if !reflect.DeepEqual(got, *fromSearch) {
			t.Errorf("Recipe(%d) =\n%+v\nsearch has\n%+v", want, got, *fromSearch)
		}
	}
}

func TestRecipeByIDWithADeniedAllergenIsNotFound(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	svc := recipeService(pool)
	ctx := context.Background()

	// Without the allergy the recipe opens, so the 404 below is the allergen.
	if _, err := svc.Recipe(ctx, seed.userID, seed.creamyPasta); err != nil {
		t.Fatalf("Recipe() without allergens error = %v", err)
	}
	if err := user.NewRepo(pool).SavePrefs(ctx, seed.userID, user.Prefs{Allergens: []string{"dairy"}}); err != nil {
		t.Fatalf("SavePrefs() error = %v", err)
	}
	if _, err := svc.Recipe(ctx, seed.userID, seed.creamyPasta); !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("Recipe() with a dairy allergy error = %v, want ErrNotFound", err)
	}
}

func TestRecipeByIDWithADislikedIngredientIsNotFound(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	svc := recipeService(pool)
	ctx := context.Background()

	if err := user.NewRepo(pool).SavePrefs(ctx, seed.userID,
		user.Prefs{DislikedIngredientIDs: []int64{seed.basilID}}); err != nil {
		t.Fatalf("SavePrefs() error = %v", err)
	}
	if _, err := svc.Recipe(ctx, seed.userID, seed.fiveShort); !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("Recipe() with disliked basil error = %v, want ErrNotFound", err)
	}
}

// A recipe that fell out of the default search still opens up to the widest
// max_missing, with the grown missing list, and not beyond it.
func TestRecipeByIDAcceptsUpToTheWidestMaxMissing(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	svc := recipeService(pool)
	ctx := context.Background()

	got, err := svc.Recipe(ctx, seed.userID, seed.fiveShort)
	if err != nil {
		t.Fatalf("Recipe(five short) error = %v", err)
	}
	if got.MissingCount != 5 || len(got.Missing) != 5 {
		t.Errorf("missing = %d (%d listed), want 5", got.MissingCount, len(got.Missing))
	}
	if _, err := svc.Recipe(ctx, seed.userID, seed.elevenShort); !errors.Is(err, httpx.ErrNotFound) {
		t.Errorf("Recipe(eleven short) error = %v, want ErrNotFound", err)
	}
	if _, err := svc.Recipe(ctx, seed.userID, 1<<40); !errors.Is(err, httpx.ErrNotFound) {
		t.Errorf("Recipe(unknown id) error = %v, want ErrNotFound", err)
	}
}

// Coverage is the caller's own: the same recipe is not a match for a user whose
// fridge has none of it.
func TestRecipeByIDIsMeasuredAgainstTheCallersFridge(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seed := seedCorpus(t, pool)
	svc := recipeService(pool)
	ctx := context.Background()
	other := pgtest.User(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO user_prefs (user_id) VALUES ($1)`, other); err != nil {
		t.Fatalf("insert prefs: %v", err)
	}

	if _, err := svc.Recipe(ctx, other, seed.chickenRice); !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("Recipe() for a user with an empty fridge error = %v, want ErrNotFound", err)
	}
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
	seed.pastaID = id("pasta")
	seed.tomatoID = id("tomato")
	seed.parmesanID = id("parmesan")

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
