//go:build integration

package cook

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/catalog"
)

// seedCatalog inserts canonical names with their allergen tags and returns their ids.
func seedCatalog(t *testing.T, pool *pgxpool.Pool, entries map[string][]string) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	ids := make(map[string]int64, len(entries))
	for name, tags := range entries {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO ingredients_catalog (canonical_name, category) VALUES ($1, 'test') RETURNING id`,
			name).Scan(&id); err != nil {
			t.Fatalf("insert ingredient %q: %v", name, err)
		}
		ids[name] = id
		for _, tag := range tags {
			if _, err := pool.Exec(ctx,
				`INSERT INTO ingredient_allergens (ingredient_id, allergen) VALUES ($1, $2)`, id, tag); err != nil {
				t.Fatalf("insert allergen %q: %v", tag, err)
			}
		}
	}
	return ids
}

func TestResolveNamesFindsTheSingularCatalogEntry(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ids := seedCatalog(t, pool, map[string][]string{
		"egg":             {"eggs"},
		"tomato":          nil,
		"cherry tomato":   nil,
		"asparagus":       nil,
		"oats":            {"gluten"},
		"brussel sprouts": nil,
		// The catalog holds both forms, each with its own tags.
		"sardines": {"fish"},
		"sardine":  nil,
	})
	repo := catalog.NewRepo(pool)

	cases := []struct {
		input, want string
		tags        []string
	}{
		{"eggs", "egg", []string{"eggs"}},
		{"  EGGS ", "egg", []string{"eggs"}},
		{"egg", "egg", []string{"eggs"}},
		{"tomatoes", "tomato", nil},
		{"cherry tomatoes", "cherry tomato", nil},
		{"brussels sprouts", "brussel sprouts", nil},
		// These look plural but are catalog names, and must not lose their "s".
		{"asparagus", "asparagus", nil},
		{"oats", "oats", []string{"gluten"}},
		// The name as written wins, so the tagged entry is not swapped for the bare one.
		{"sardines", "sardines", []string{"fish"}},
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
			if got.Ingredient.CanonicalName != tc.want || got.Ingredient.ID != ids[tc.want] {
				t.Errorf("resolved to %q (id %d), want %q (id %d)",
					got.Ingredient.CanonicalName, got.Ingredient.ID, tc.want, ids[tc.want])
			}
			if !got.Exact {
				t.Error("Exact = false, want true: this is not a similarity guess")
			}
			if !slices.Equal(got.Ingredient.Allergens, tc.tags) {
				t.Errorf("allergens = %v, want %v", got.Ingredient.Allergens, tc.tags)
			}
		})
	}
}

// These names were untagged, so resolving their plurals would have let them past a
// deny-list that used to reject them as unverifiable.
func TestGenericIngredientsCarryAllergensFromMigrations(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()

	const baseID = 1
	v := NewValidator(catalog.NewRepo(pool))
	cases := []struct {
		name string
		deny []string
	}{
		{"nuts", []string{"tree_nuts"}},
		{"nuts", []string{"peanuts"}},
		{"nut", []string{"tree_nuts"}},
		{"pastries", []string{"gluten"}},
		{"pastry", []string{"wheat"}},
		{"all purpose flours", []string{"wheat"}},
		{"all purpose flour", []string{"gluten"}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.deny[0], func(t *testing.T) {
			_, err := v.Validate(context.Background(), ValidationInput{
				CandidateIDs:  []int64{baseID},
				Bases:         map[int64]catalog.Recipe{baseID: {ID: baseID, Title: "Pie"}},
				DenyAllergens: tc.deny,
			}, LLMAdaptedRecipe{
				BaseRecipeID:    baseID,
				Title:           "Pie",
				Steps:           []string{"Bake."},
				UsedIngredients: []string{tc.name},
			})
			rej, ok := AsRejection(err)
			if !ok || rej.Reason != RejectAllergen {
				t.Fatalf("Validate() error = %v, want an allergen rejection", err)
			}
		})
	}

	// A load upserts the catalog; it must keep the tags.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO ingredients_catalog (canonical_name, category) VALUES ('nut', 'nuts')
ON CONFLICT (lower(canonical_name)) DO UPDATE
    SET category = COALESCE(excluded.category, ingredients_catalog.category)`); err != nil {
		t.Fatalf("upsert catalog: %v", err)
	}
	resolved, _, err := catalog.NewRepo(pool).ResolveNames(context.Background(), []string{"nuts"})
	if err != nil {
		t.Fatalf("ResolveNames() error = %v", err)
	}
	if len(resolved) != 1 || !slices.Equal(resolved[0].Ingredient.Allergens, []string{"peanuts", "tree_nuts"}) {
		t.Errorf("resolved = %+v, want nut with peanuts and tree_nuts", resolved)
	}
}

// The singular form is compared with canonical names only. Synonyms stay out of it:
// some of them drop an allergen (TASK-03), and this must not open a new way to reach them.
func TestResolveNamesDoesNotReachSynonymsThroughTheSingular(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ids := seedCatalog(t, pool, map[string][]string{"oil": nil})
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO ingredient_synonyms (ingredient_id, synonym) VALUES ($1, 'sardine in oil')`, ids["oil"]); err != nil {
		t.Fatalf("insert synonym: %v", err)
	}

	resolved, unresolved, err := catalog.NewRepo(pool).ResolveNames(ctx, []string{"sardines in oil"})
	if err != nil {
		t.Fatalf("ResolveNames() error = %v", err)
	}
	if len(resolved) != 0 || !slices.Equal(unresolved, []string{"sardines in oil"}) {
		t.Errorf("resolved = %v, unresolved = %v, want the name left unresolved", resolved, unresolved)
	}
}

func TestResolveNamesLeavesUnknownPluralsUnresolved(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seedCatalog(t, pool, map[string][]string{"beef": nil, "egg": {"eggs"}})

	resolved, unresolved, err := catalog.NewRepo(pool).ResolveNames(context.Background(), []string{"beers", "citrus"})
	if err != nil {
		t.Fatalf("ResolveNames() error = %v", err)
	}
	if len(resolved) != 0 || !slices.Equal(unresolved, []string{"beers", "citrus"}) {
		t.Errorf("resolved = %v, unresolved = %v, want both unresolved", resolved, unresolved)
	}
}

// A plural name must meet the allergen check by id, not merely stop being unverifiable.
func TestValidateRejectsPluralNameOfADeniedAllergen(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	seedCatalog(t, pool, map[string][]string{"egg": {"eggs"}, "tomato": nil})

	const baseID = 1
	in := ValidationInput{
		CandidateIDs: []int64{baseID},
		Bases:        map[int64]catalog.Recipe{baseID: {ID: baseID, Title: "Omelette"}},
	}
	out := LLMAdaptedRecipe{
		BaseRecipeID:    baseID,
		Title:           "Omelette",
		Steps:           []string{"Beat the eggs."},
		UsedIngredients: []string{"eggs", "tomatoes"},
	}
	v := NewValidator(catalog.NewRepo(pool))
	ctx := context.Background()

	result, err := v.Validate(ctx, in, out)
	if err != nil {
		t.Fatalf("Validate() without a deny-list error = %v", err)
	}
	if len(result.Used)+len(result.Missing) != 2 {
		t.Errorf("used = %v, missing = %v, want both names on the result", result.Used, result.Missing)
	}

	in.DenyAllergens = []string{"eggs"}
	_, err = v.Validate(ctx, in, out)
	rej, ok := AsRejection(err)
	if !ok || rej.Reason != RejectAllergen {
		t.Fatalf("Validate() error = %v, want an allergen rejection", err)
	}
}
