package cook

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RetrievalRepo runs the set-coverage search. This is the main request path and
// it never involves the model.
type RetrievalRepo struct {
	pool *pgxpool.Pool
}

func NewRetrievalRepo(pool *pgxpool.Pool) *RetrievalRepo {
	return &RetrievalRepo{pool: pool}
}

// Query is the fully resolved retrieval input.
type Query struct {
	UserID         uuid.UUID
	Allergens      []string
	DislikedIDs    []int64
	MaxMissing     int
	MaxSteps       int
	MaxIngredients int
	Categories     []string
	Limit          int
	UseSoonBefore  time.Time
}

// coverageRow is the raw ranking output before ingredient detail is attached.
type coverageRow struct {
	RecipeID     int64
	TotalCount   int
	MissingCount int
	UseSoonCount int
}

// setCoverageSQL is the heart of the product.
//
// It reads recipes.ingredient_ids, a trigger-maintained int[] copy of the
// recipe_ingredients links (migration 00007), and counts coverage with intarray's
// C intersection (`&`, icount). Frequent fridge items make almost the whole corpus
// a candidate, so the per-recipe count has to be cheap: the aggregate over the
// link table, and later a subquery per recipe, kept p95 above 150 ms (TASK-39).
//
// `have` is the fridge plus pantry staples. A recipe is a candidate only if it uses
// at least one thing actually in the fridge: that is the product definition of a
// match ("cook from what I have"). Allergens and dislikes are excluded by overlap
// on canonical ids rather than by text matching, so an allergy cannot be defeated
// by a spelling difference. The denied ids come from ingredient_allergens on every
// call, so the tags themselves are never copied and cannot go stale.
//
// Every array is built sorted. intarray sorts both operands on each call, and
// Postgres' sort returns at once on sorted input, so an unsorted deny-list of a
// few hundred ids made the allergen overlap the most expensive step of the query.
//
// The arrays are int4 because intarray is. The triggers refuse to store an id
// that does not fit, so an id above that range is never in ingredient_ids and is
// left out of the query arrays rather than cast into an error.
//
// Ranking is missing_count then total_count; the near-expiry count and step count
// are tie-breakers below those.
const setCoverageSQL = `
WITH fridge AS (
    SELECT coalesce(array_agg(ingredient_id::int ORDER BY ingredient_id) FILTER (WHERE ingredient_id <= 2147483647), '{}') AS ids
    FROM user_ingredients WHERE user_id = $1
),
have AS (
    SELECT coalesce(array_agg(ingredient_id::int ORDER BY ingredient_id) FILTER (WHERE ingredient_id <= 2147483647), '{}') AS ids
    FROM (
        SELECT ingredient_id FROM user_ingredients WHERE user_id = $1
        UNION
        SELECT ingredient_id FROM pantry_staples
    ) h
),
use_soon AS (
    SELECT coalesce(array_agg(ingredient_id::int ORDER BY ingredient_id) FILTER (WHERE ingredient_id <= 2147483647), '{}') AS ids
    FROM user_ingredients
    WHERE user_id = $1 AND expires_at IS NOT NULL AND expires_at <= $8::date
),
denied AS (
    SELECT coalesce(array_agg(ingredient_id::int ORDER BY ingredient_id) FILTER (WHERE ingredient_id <= 2147483647), '{}') AS ids
    FROM ingredient_allergens WHERE allergen = ANY($2::text[])
),
disliked AS (
    SELECT coalesce(array_agg(id::int ORDER BY id) FILTER (WHERE id <= 2147483647), '{}') AS ids
    FROM unnest($7::bigint[]) id
),
coverage AS (
    SELECT r.id AS recipe_id, r.num_steps,
           icount(r.ingredient_ids) AS total_count,
           icount(r.ingredient_ids) - icount(r.ingredient_ids & have.ids) AS missing_count,
           icount(r.ingredient_ids & use_soon.ids) AS use_soon_count
    FROM recipes r, fridge, have, use_soon, denied, disliked
    WHERE r.ingredient_ids && fridge.ids
      AND ($4 <= 0 OR r.num_steps <= $4)
      AND ($5 <= 0 OR r.num_ingredients <= $5)
      AND (cardinality($6::text[]) = 0 OR r.category = ANY($6::text[]))
      AND NOT (r.ingredient_ids && denied.ids)
      AND NOT (r.ingredient_ids && disliked.ids)
)
SELECT recipe_id, total_count, missing_count, use_soon_count
FROM coverage
WHERE missing_count <= $3
ORDER BY missing_count ASC, total_count ASC,
         use_soon_count DESC, num_steps ASC, recipe_id ASC
LIMIT $9`

func (r *RetrievalRepo) Coverage(ctx context.Context, q Query) ([]coverageRow, error) {
	allergens := q.Allergens
	if allergens == nil {
		allergens = []string{}
	}
	disliked := q.DislikedIDs
	if disliked == nil {
		disliked = []int64{}
	}
	categories := q.Categories
	if categories == nil {
		categories = []string{}
	}

	rows, err := r.pool.Query(ctx, setCoverageSQL,
		q.UserID, allergens, q.MaxMissing, q.MaxSteps, q.MaxIngredients,
		categories, disliked, q.UseSoonBefore, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("set-coverage query: %w", err)
	}
	defer rows.Close()

	var out []coverageRow
	for rows.Next() {
		var row coverageRow
		if err := rows.Scan(&row.RecipeID, &row.TotalCount, &row.MissingCount, &row.UseSoonCount); err != nil {
			return nil, fmt.Errorf("scan coverage row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// IsRetrievableFor reports whether a recipe passes the same missing-count,
// allergen and dislike checks as retrieval. Paging and the category and
// complexity filters are ignored. Adaptation uses it to reject requests for
// recipes the user could not have been offered.
func (r *RetrievalRepo) IsRetrievableFor(ctx context.Context, q Query, recipeID int64) (bool, error) {
	allergens := q.Allergens
	if allergens == nil {
		allergens = []string{}
	}
	disliked := q.DislikedIDs
	if disliked == nil {
		disliked = []int64{}
	}
	var ok bool
	err := r.pool.QueryRow(ctx, `
WITH have AS (
    SELECT ingredient_id FROM user_ingredients WHERE user_id = $1
    UNION
    SELECT ingredient_id FROM pantry_staples
)
SELECT EXISTS (
    SELECT 1
    FROM recipe_ingredients ri
    LEFT JOIN have h ON h.ingredient_id = ri.ingredient_id
    WHERE ri.recipe_id = $3
    GROUP BY ri.recipe_id
    HAVING count(*) FILTER (WHERE h.ingredient_id IS NULL) <= $4
) AND NOT EXISTS (
    SELECT 1 FROM recipe_ingredients x
    JOIN ingredient_allergens a ON a.ingredient_id = x.ingredient_id
    WHERE x.recipe_id = $3 AND a.allergen = ANY($2::text[])
) AND NOT EXISTS (
    SELECT 1 FROM recipe_ingredients d
    WHERE d.recipe_id = $3 AND d.ingredient_id = ANY($5::bigint[])
)`, q.UserID, allergens, recipeID, q.MaxMissing, disliked).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check recipe retrievability: %w", err)
	}
	return ok, nil
}

// HaveIngredientIDs is the fridge plus staples, which is what "missing" is
// measured against everywhere else in this package.
func (r *RetrievalRepo) HaveIngredientIDs(ctx context.Context, userID uuid.UUID) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
SELECT ingredient_id FROM user_ingredients WHERE user_id = $1
UNION
SELECT ingredient_id FROM pantry_staples`, userID)
	if err != nil {
		return nil, fmt.Errorf("select have ingredients: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan have ingredient: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
