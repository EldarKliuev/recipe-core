package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
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

func (r *Repo) Prefs(ctx context.Context, userID uuid.UUID) (Prefs, error) {
	var p Prefs
	err := r.pool.QueryRow(ctx,
		`SELECT allergens, diets, disliked_ingredient_ids, cuisines
		 FROM user_prefs WHERE user_id = $1`, userID).
		Scan(&p.Allergens, &p.Diets, &p.DislikedIngredientIDs, &p.Cuisines)
	if errors.Is(err, pgx.ErrNoRows) {
		// Registration creates the row, so a miss means the user is gone.
		return p, fmt.Errorf("%w: user prefs", httpx.ErrNotFound)
	}
	if err != nil {
		return p, fmt.Errorf("select user prefs: %w", err)
	}
	return p, nil
}

// SavePrefs upserts the row. Nil slices are written as empty arrays: the columns
// are NOT NULL, a nil slice binds as SQL NULL, and "no allergens" is an empty list
// rather than an error. Validate normalises this too, but the repo must not depend
// on every caller having run it first.
func (r *Repo) SavePrefs(ctx context.Context, userID uuid.UUID, p Prefs) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO user_prefs (user_id, allergens, diets, disliked_ingredient_ids, cuisines, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (user_id) DO UPDATE SET
    allergens = excluded.allergens,
    diets = excluded.diets,
    disliked_ingredient_ids = excluded.disliked_ingredient_ids,
    cuisines = excluded.cuisines,
    updated_at = now()`,
		userID,
		orEmpty(p.Allergens), orEmpty(p.Diets),
		orEmpty(p.DislikedIngredientIDs), orEmpty(p.Cuisines))
	if err != nil {
		return fmt.Errorf("upsert user prefs: %w", err)
	}
	return nil
}

func orEmpty[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
