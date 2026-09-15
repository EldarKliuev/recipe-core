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
	chickenID   int64
	saltID      int64
}

// testPool returns a pool against TEST_DATABASE_URL if set, otherwise it starts a
// throwaway Postgres container. Reusing an existing database is much faster in a
// tight edit-test loop.
func testPool(t *testing.T) (*pgxpool.Pool, func()) {
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
		up := upSection(string(body))
		if _, err := pool.Exec(ctx, up); err != nil {
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

func seedCorpus(t *testing.T, pool *pgxpool.Pool) corpusSeed {
	t.Helper()
	ctx := context.Background()
	seed := corpusSeed{userID: uuid.New()}

	ingredients := map[string]*int64{}
	for _, name := range []string{"chicken breast", "rice", "salt", "heavy cream", "pasta",
		"tomato", "onion", "garlic", "basil", "parmesan", "saffron", "truffle"} {
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

	return seed
}
