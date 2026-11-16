//go:build integration

package cook

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/recipe-ai/core-api/migrations"
)

// seedStapleNames are the catalog rows 00005_seed_staples.sql inserts.
var seedStapleNames = []string{
	"water", "salt", "black pepper", "sugar", "vegetable oil", "olive oil",
	"all-purpose flour", "baking powder", "baking soda", "white vinegar", "butter",
}

func TestMigrationDownTo4KeepsSeedRowsThatTheCorpusUses(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	db := gooseFromScratch(t, pool)

	// A loaded database: a recipe uses salt and butter, a fridge holds olive oil.
	var recipeID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO recipes (title) VALUES ('buttered toast') RETURNING id`).Scan(&recipeID); err != nil {
		t.Fatalf("insert recipe: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recipe_ingredients (recipe_id, ingredient_id, raw_text)
		 SELECT $1, id, canonical_name FROM ingredients_catalog WHERE canonical_name IN ('salt', 'butter')`,
		recipeID); err != nil {
		t.Fatalf("insert recipe ingredients: %v", err)
	}
	userID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("down-%s@example.com", userID)); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_ingredients (user_id, ingredient_id)
		 SELECT $1, id FROM ingredients_catalog WHERE canonical_name = 'olive oil'`, userID); err != nil {
		t.Fatalf("insert fridge item: %v", err)
	}

	if err := goose.DownToContext(ctx, db, ".", 4); err != nil {
		t.Fatalf("down to 4 on a loaded database: %v", err)
	}

	present := seedRowsPresent(t, pool)
	for _, name := range []string{"salt", "butter", "olive oil"} {
		if !present[name] {
			t.Errorf("%q is referenced but was deleted", name)
		}
	}
	if present["baking soda"] {
		t.Errorf("unreferenced %q survived the rollback", "baking soda")
	}

	var butterIsDairy, saltIsStaple bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ingredient_allergens a JOIN ingredients_catalog c ON c.id = a.ingredient_id
		                WHERE c.canonical_name = 'butter' AND a.allergen = 'dairy'),
		        EXISTS (SELECT 1 FROM pantry_staples s JOIN ingredients_catalog c ON c.id = s.ingredient_id
		                WHERE c.canonical_name = 'salt')`).Scan(&butterIsDairy, &saltIsStaple); err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if !butterIsDairy {
		t.Error("butter used by a recipe lost its dairy tag")
	}
	if !saltIsStaple {
		t.Error("salt used by a recipe stopped being a staple")
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("up again after the rollback: %v", err)
	}
	if got := seedRowsPresent(t, pool); len(got) != len(seedStapleNames) {
		t.Errorf("after up again %d of %d seed rows are present", len(got), len(seedStapleNames))
	}
}

func TestMigrationDownTo4RemovesTheSeedFromAnEmptyCorpus(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	db := gooseFromScratch(t, pool)

	if err := goose.DownToContext(ctx, db, ".", 4); err != nil {
		t.Fatalf("down to 4: %v", err)
	}
	if got := seedRowsPresent(t, pool); len(got) != 0 {
		t.Errorf("seed rows left after the rollback: %v", got)
	}
	var staples, allergens int
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM pantry_staples), (SELECT count(*) FROM ingredient_allergens)`).
		Scan(&staples, &allergens); err != nil {
		t.Fatalf("count: %v", err)
	}
	if staples != 0 || allergens != 0 {
		t.Errorf("after the rollback %d staples and %d allergen tags remain, want none", staples, allergens)
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := seedRowsPresent(t, pool); len(got) != len(seedStapleNames) {
		t.Errorf("after up again %d of %d seed rows are present", len(got), len(seedStapleNames))
	}
}

// gooseFromScratch drops the schema testPool prepared and applies every migration
// through goose itself, so the Down sections run exactly as `make migrate-down` runs them.
func gooseFromScratch(t *testing.T, pool *pgxpool.Pool) *sql.DB {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpContext(context.Background(), db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	return db
}

func seedRowsPresent(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT canonical_name FROM ingredients_catalog WHERE canonical_name = ANY($1)`, seedStapleNames)
	if err != nil {
		t.Fatalf("query seed rows: %v", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return present
}
