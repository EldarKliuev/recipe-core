package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

func (r *Repo) List(ctx context.Context, userID uuid.UUID) ([]Item, error) {
	rows, err := r.pool.Query(ctx, `
SELECT ui.ingredient_id, c.canonical_name, c.category, ui.qty, ui.unit, ui.expires_at
FROM user_ingredients ui
JOIN ingredients_catalog c ON c.id = ui.ingredient_id
WHERE ui.user_id = $1
ORDER BY (ui.expires_at IS NULL), ui.expires_at, c.canonical_name`, userID)
	if err != nil {
		return nil, fmt.Errorf("select inventory: %w", err)
	}
	defer rows.Close()

	cutoff := time.Now().Add(UseSoonWindow)
	items := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.IngredientID, &it.CanonicalName, &it.Category, &it.Qty, &it.Unit, &it.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan inventory item: %w", err)
		}
		it.UseSoon = it.ExpiresAt != nil && it.ExpiresAt.Before(cutoff)
		items = append(items, it)
	}
	return items, rows.Err()
}

// Upsert adds or replaces one fridge entry. Re-adding an ingredient updates
// quantity and expiry rather than failing, which is what the UI needs.
func (r *Repo) Upsert(ctx context.Context, userID uuid.UUID, ingredientID int64, qty *float64, unit *string, expiresAt *time.Time) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO user_ingredients (user_id, ingredient_id, qty, unit, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, ingredient_id) DO UPDATE SET
    qty = excluded.qty, unit = excluded.unit, expires_at = excluded.expires_at`,
		userID, ingredientID, qty, unit, expiresAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("%w: ingredient %d is not in the catalog", httpx.ErrInvalidInput, ingredientID)
		}
		return fmt.Errorf("upsert inventory item: %w", err)
	}
	return nil
}

const foreignKeyViolation = "23503"

func (r *Repo) Delete(ctx context.Context, userID uuid.UUID, ingredientID int64) error {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM user_ingredients WHERE user_id = $1 AND ingredient_id = $2`,
		userID, ingredientID)
	if err != nil {
		return fmt.Errorf("delete inventory item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: ingredient %d is not in your fridge", httpx.ErrNotFound, ingredientID)
	}
	return nil
}

// IngredientIDs is the id-only view used by retrieval.
func (r *Repo) IngredientIDs(ctx context.Context, userID uuid.UUID) ([]int64, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT ingredient_id FROM user_ingredients WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("select inventory ids: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan inventory id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
