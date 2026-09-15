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
// `have` is the fridge plus pantry staples. `candidates` narrows to recipes that
// use at least one thing actually in the fridge: that is the product definition
// of a match ("cook from what I have"), and it also keeps the aggregate off a
// full scan of recipe_ingredients. Allergens and dislikes are excluded with NOT
// EXISTS on canonical ids rather than by text matching, so an allergy cannot be
// defeated by a spelling difference.
//
// Ranking is missing_count then total_count; the near-expiry count and step count
// are tie-breakers below those. If this stops meeting the p95 < 150 ms target on
// ~600k ingredient rows, the documented next step is denormalising into
// recipes.ingredient_ids int[] with a GIN index and computing coverage with array
// operators, not reaching for a vector index.
const setCoverageSQL = `
WITH have AS (
    SELECT ingredient_id FROM user_ingredients WHERE user_id = $1
    UNION
    SELECT ingredient_id FROM pantry_staples
),
use_soon AS (
    SELECT ingredient_id FROM user_ingredients
    WHERE user_id = $1 AND expires_at IS NOT NULL AND expires_at <= $8::date
),
candidates AS (
    SELECT DISTINCT ri.recipe_id
    FROM user_ingredients ui
    JOIN recipe_ingredients ri ON ri.ingredient_id = ui.ingredient_id
    WHERE ui.user_id = $1
),
coverage AS (
    SELECT ri.recipe_id,
           count(*) AS total_count,
           count(*) FILTER (WHERE h.ingredient_id IS NULL) AS missing_count,
           count(*) FILTER (WHERE s.ingredient_id IS NOT NULL) AS use_soon_count
    FROM candidates cand
    JOIN recipe_ingredients ri ON ri.recipe_id = cand.recipe_id
    LEFT JOIN have h ON h.ingredient_id = ri.ingredient_id
    LEFT JOIN use_soon s ON s.ingredient_id = ri.ingredient_id
    GROUP BY ri.recipe_id
    HAVING count(*) FILTER (WHERE h.ingredient_id IS NULL) <= $3
)
SELECT cov.recipe_id, cov.total_count, cov.missing_count, cov.use_soon_count
FROM coverage cov
JOIN recipes r ON r.id = cov.recipe_id
WHERE ($4 <= 0 OR r.num_steps <= $4)
  AND ($5 <= 0 OR r.num_ingredients <= $5)
  AND (cardinality($6::text[]) = 0 OR r.category = ANY($6::text[]))
  AND NOT EXISTS (
      SELECT 1 FROM recipe_ingredients x
      JOIN ingredient_allergens a ON a.ingredient_id = x.ingredient_id
      WHERE x.recipe_id = cov.recipe_id AND a.allergen = ANY($2::text[])
  )
  AND NOT EXISTS (
      SELECT 1 FROM recipe_ingredients d
      WHERE d.recipe_id = cov.recipe_id AND d.ingredient_id = ANY($7::bigint[])
  )
ORDER BY cov.missing_count ASC, cov.total_count ASC,
         cov.use_soon_count DESC, r.num_steps ASC, r.id ASC
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

// IsRetrievableFor reports whether a recipe would be returned by retrieval for
// this user, ignoring paging limits. Adaptation uses it to reject requests for
// recipes the user was never offered.
func (r *RetrievalRepo) IsRetrievableFor(ctx context.Context, q Query, recipeID int64) (bool, error) {
	allergens := q.Allergens
	if allergens == nil {
		allergens = []string{}
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
)`, q.UserID, allergens, recipeID, q.MaxMissing).Scan(&ok)
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
