package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

const ingredientSelect = `
SELECT c.id, c.canonical_name, c.category,
       coalesce(array_agg(a.allergen) FILTER (WHERE a.allergen IS NOT NULL), '{}') AS allergens,
       (s.ingredient_id IS NOT NULL) AS is_staple
FROM ingredients_catalog c
LEFT JOIN ingredient_allergens a ON a.ingredient_id = c.id
LEFT JOIN pantry_staples s ON s.ingredient_id = c.id`

// Search powers the autocomplete endpoint. Prefix hits rank above substring
// hits, which rank above fuzzy trigram hits, so typing "chic" puts "chicken"
// first instead of an arbitrary similar name.
func (r *Repo) Search(ctx context.Context, query string, limit int) ([]Ingredient, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	rows, err := r.pool.Query(ctx, `
WITH matches AS (
    SELECT c.id,
           CASE
               WHEN lower(c.canonical_name) = $1 THEN 0
               WHEN lower(c.canonical_name) LIKE $1 || '%' THEN 1
               WHEN lower(c.canonical_name) LIKE '%' || $1 || '%' THEN 2
               WHEN EXISTS (
                   SELECT 1 FROM ingredient_synonyms s
                   WHERE s.ingredient_id = c.id AND lower(s.synonym) LIKE $1 || '%'
               ) THEN 3
               ELSE 4
           END AS rank,
           similarity(lower(c.canonical_name), $1) AS sim
    FROM ingredients_catalog c
    WHERE lower(c.canonical_name) LIKE '%' || $1 || '%'
       OR similarity(lower(c.canonical_name), $1) > 0.3
       OR EXISTS (
           SELECT 1 FROM ingredient_synonyms s
           WHERE s.ingredient_id = c.id AND lower(s.synonym) LIKE '%' || $1 || '%'
       )
)`+ingredientSelect+`
JOIN matches m ON m.id = c.id
GROUP BY c.id, c.canonical_name, c.category, s.ingredient_id, m.rank, m.sim
ORDER BY m.rank, m.sim DESC, length(c.canonical_name), c.canonical_name
LIMIT $2`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("search ingredients: %w", err)
	}
	defer rows.Close()
	return scanIngredients(rows)
}

func (r *Repo) IngredientsByIDs(ctx context.Context, ids []int64) ([]Ingredient, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, ingredientSelect+`
WHERE c.id = ANY($1::bigint[])
GROUP BY c.id, c.canonical_name, c.category, s.ingredient_id
ORDER BY c.canonical_name`, ids)
	if err != nil {
		return nil, fmt.Errorf("select ingredients by id: %w", err)
	}
	defer rows.Close()
	return scanIngredients(rows)
}

// ExistingIngredientIDs returns the subset of ids present in the catalog, so
// callers can reject references to ingredients that do not exist.
func (r *Repo) ExistingIngredientIDs(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id FROM ingredients_catalog WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("select existing ingredient ids: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan ingredient id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResolveNames maps free-text names to canonical ingredients. Names that match
// nothing are returned in unresolved so the caller can report them instead of
// silently dropping fridge contents.
func (r *Repo) ResolveNames(ctx context.Context, names []string) (resolved []Resolution, unresolved []string, err error) {
	for _, name := range names {
		res, found, err := r.resolveOne(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			unresolved = append(unresolved, name)
			continue
		}
		resolved = append(resolved, res)
	}
	return resolved, unresolved, nil
}

// fuzzyThreshold is deliberately high: mapping "beef" onto "beer" because of
// trigram overlap would silently corrupt someone's fridge.
const fuzzyThreshold = 0.55

// pluralExceptions mirrors _PLURAL_EXCEPTIONS in tools/ingest/ingest/parser.py.
var pluralExceptions = map[string]string{
	"leaves": "leaf", "loaves": "loaf", "halves": "half", "knives": "knife",
	"tomatoes": "tomato", "potatoes": "potato", "berries": "berry",
	"cherries": "cherry", "strawberries": "strawberry",
	"blueberries": "blueberry", "raspberries": "raspberry",
	"cranberries": "cranberry", "anchovies": "anchovy",
	"molasses": "molasses", "asparagus": "asparagus", "hummus": "hummus",
	"couscous": "couscous", "swiss": "swiss", "bass": "bass",
	"watercress": "watercress", "grits": "grits", "greens": "greens",
	"oats": "oats", "chives": "chives", "capers": "capers", "peas": "peas",
	"beans": "beans", "lentils": "lentils", "noodles": "noodles",
	"sprouts": "sprouts", "grapes": "grapes", "raisins": "raisins",
	"breadcrumbs": "breadcrumbs", "cloves": "clove",
}

// singularName applies the rule ingest used to build canonical names, so a plural
// reaches the entry its corpus lines were linked to. It mirrors singularise in
// tools/ingest/ingest/parser.py, including its odd results: "peaches" becomes
// "peache" because that is the name in the catalog.
func singularName(name string) string {
	words := strings.Fields(name)
	for i, word := range words {
		words[i] = singularWord(word)
	}
	return strings.Join(words, " ")
}

func singularWord(word string) string {
	if singular, ok := pluralExceptions[word]; ok {
		return singular
	}
	if len(word) <= 3 {
		return word
	}
	if strings.HasSuffix(word, "ies") {
		return strings.TrimSuffix(word, "ies") + "y"
	}
	if strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") {
		return strings.TrimSuffix(word, "s")
	}
	return word
}

func (r *Repo) resolveOne(ctx context.Context, name string) (Resolution, bool, error) {
	q := strings.ToLower(strings.TrimSpace(name))
	if q == "" {
		return Resolution{}, false, nil
	}
	var (
		ing   Ingredient
		exact bool
	)
	err := r.pool.QueryRow(ctx, `
WITH candidate AS (
    SELECT c.id, 0 AS rank, 1.0 AS sim
    FROM ingredients_catalog c WHERE lower(c.canonical_name) = $1
    UNION ALL
    SELECT s.ingredient_id, 1, 1.0
    FROM ingredient_synonyms s WHERE lower(s.synonym) = $1
    UNION ALL
    -- Canonical names only: some synonyms drop an allergen, and the singular must
    -- not become another way to reach them.
    SELECT c.id, 2, 1.0
    FROM ingredients_catalog c WHERE lower(c.canonical_name) = $3
    UNION ALL
    SELECT c.id, 3, similarity(lower(c.canonical_name), $1)
    FROM ingredients_catalog c
    WHERE similarity(lower(c.canonical_name), $1) >= $2
)
SELECT c.id, c.canonical_name, c.category,
       coalesce(array_agg(DISTINCT a.allergen) FILTER (WHERE a.allergen IS NOT NULL), '{}'),
       (st.ingredient_id IS NOT NULL),
       (min(cand.rank) < 3)
FROM candidate cand
JOIN ingredients_catalog c ON c.id = cand.id
LEFT JOIN ingredient_allergens a ON a.ingredient_id = c.id
LEFT JOIN pantry_staples st ON st.ingredient_id = c.id
GROUP BY c.id, c.canonical_name, c.category, st.ingredient_id
ORDER BY min(cand.rank), max(cand.sim) DESC, length(c.canonical_name)
LIMIT 1`, q, fuzzyThreshold, singularName(q)).
		Scan(&ing.ID, &ing.CanonicalName, &ing.Category, &ing.Allergens, &ing.IsStaple, &exact)
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolution{}, false, nil
	}
	if err != nil {
		return Resolution{}, false, fmt.Errorf("resolve ingredient %q: %w", name, err)
	}
	return Resolution{Input: name, Ingredient: ing, Exact: exact}, true, nil
}

func (r *Repo) RecipeByID(ctx context.Context, id int64) (Recipe, error) {
	var rec Recipe
	err := r.pool.QueryRow(ctx, `
SELECT id, title, category, subcategory, description, num_ingredients, num_steps, source, created_at
FROM recipes WHERE id = $1`, id).
		Scan(&rec.ID, &rec.Title, &rec.Category, &rec.Subcategory, &rec.Description,
			&rec.NumIngredients, &rec.NumSteps, &rec.Source, &rec.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, fmt.Errorf("%w: recipe %d", httpx.ErrNotFound, id)
	}
	if err != nil {
		return rec, fmt.Errorf("select recipe: %w", err)
	}
	return rec, nil
}

// RecipesByIDs returns corpus metadata keyed by id, preserving nothing about
// ordering; callers order by their own ranking.
func (r *Repo) RecipesByIDs(ctx context.Context, ids []int64) (map[int64]Recipe, error) {
	if len(ids) == 0 {
		return map[int64]Recipe{}, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT id, title, category, subcategory, description, num_ingredients, num_steps, source, created_at
FROM recipes WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("select recipes by id: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]Recipe, len(ids))
	for rows.Next() {
		var rec Recipe
		if err := rows.Scan(&rec.ID, &rec.Title, &rec.Category, &rec.Subcategory, &rec.Description,
			&rec.NumIngredients, &rec.NumSteps, &rec.Source, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan recipe: %w", err)
		}
		out[rec.ID] = rec
	}
	return out, rows.Err()
}

// IngredientsForRecipes returns the ingredient lines of several recipes at once,
// keyed by recipe id.
func (r *Repo) IngredientsForRecipes(ctx context.Context, recipeIDs []int64) (map[int64][]RecipeIngredient, error) {
	if len(recipeIDs) == 0 {
		return map[int64][]RecipeIngredient{}, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT ri.recipe_id, ri.ingredient_id, c.canonical_name, ri.raw_text, ri.qty, ri.unit
FROM recipe_ingredients ri
JOIN ingredients_catalog c ON c.id = ri.ingredient_id
WHERE ri.recipe_id = ANY($1::bigint[])
ORDER BY ri.recipe_id, c.canonical_name`, recipeIDs)
	if err != nil {
		return nil, fmt.Errorf("select recipe ingredients: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]RecipeIngredient, len(recipeIDs))
	for rows.Next() {
		var recipeID int64
		var ri RecipeIngredient
		if err := rows.Scan(&recipeID, &ri.IngredientID, &ri.CanonicalName, &ri.RawText, &ri.Qty, &ri.Unit); err != nil {
			return nil, fmt.Errorf("scan recipe ingredient: %w", err)
		}
		out[recipeID] = append(out[recipeID], ri)
	}
	return out, rows.Err()
}

// Directions returns the verbatim corpus steps. Internal use only: they are fed
// to llm-service for paraphrasing and must never reach an API response.
func (r *Repo) Directions(ctx context.Context, recipeIDs []int64) (map[int64][]string, error) {
	if len(recipeIDs) == 0 {
		return map[int64][]string{}, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id, steps FROM recipes WHERE id = ANY($1::bigint[])`, recipeIDs)
	if err != nil {
		return nil, fmt.Errorf("select directions: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]string, len(recipeIDs))
	for rows.Next() {
		var id int64
		var steps []string
		if err := rows.Scan(&id, &steps); err != nil {
			return nil, fmt.Errorf("scan directions: %w", err)
		}
		out[id] = steps
	}
	return out, rows.Err()
}

// AllergensForIngredients maps ingredient id to its allergen tags, used by the
// validator's deny-list check.
func (r *Repo) AllergensForIngredients(ctx context.Context, ids []int64) (map[int64][]string, error) {
	if len(ids) == 0 {
		return map[int64][]string{}, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT ingredient_id, allergen FROM ingredient_allergens WHERE ingredient_id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("select allergens: %w", err)
	}
	defer rows.Close()

	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var allergen string
		if err := rows.Scan(&id, &allergen); err != nil {
			return nil, fmt.Errorf("scan allergen: %w", err)
		}
		out[id] = append(out[id], allergen)
	}
	return out, rows.Err()
}

func scanIngredients(rows pgx.Rows) ([]Ingredient, error) {
	var out []Ingredient
	for rows.Next() {
		var ing Ingredient
		if err := rows.Scan(&ing.ID, &ing.CanonicalName, &ing.Category, &ing.Allergens, &ing.IsStaple); err != nil {
			return nil, fmt.Errorf("scan ingredient: %w", err)
		}
		out = append(out, ing)
	}
	return out, rows.Err()
}
