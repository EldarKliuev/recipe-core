package recipehistory

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

func (r *Repo) SaveAdapted(ctx context.Context, a Entry) error {
	var model *string
	if a.Model != nil && *a.Model != "" {
		model = a.Model
	}
	_, err := r.pool.Exec(ctx, `
INSERT INTO adapted_recipes (id, user_id, base_recipe_id, result, model, adapted)
VALUES ($1, $2, $3, $4, $5, $6)`,
		a.ID, a.UserID, a.BaseRecipeID, []byte(a.Result), model, a.Adapted)
	if err != nil {
		return fmt.Errorf("insert adapted recipe: %w", err)
	}
	return nil
}

func (r *Repo) SaveRun(ctx context.Context, run Run) error {
	id := run.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := r.pool.Exec(ctx, `
INSERT INTO generation_runs
    (id, user_id, job_id, model, prompt_version, latency_ms, prompt_tokens, output_tokens, retries, status, error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, run.UserID, run.JobID, nullIfEmpty(run.Model), nullIfEmpty(run.PromptVersion),
		run.LatencyMS, run.PromptTokens, run.OutputTokens, run.Retries, run.Status, nullIfEmpty(run.Error))
	if err != nil {
		return fmt.Errorf("insert generation run: %w", err)
	}
	return nil
}

// ListItem is a history entry joined with the corpus title of its base recipe.
type ListItem struct {
	Entry
	BaseTitle string `json:"base_title"`
}

func (r *Repo) List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]ListItem, error) {
	rows, err := r.pool.Query(ctx, `
SELECT ar.id, ar.base_recipe_id, ar.result, ar.model, ar.adapted, ar.rating, ar.created_at, rc.title
FROM adapted_recipes ar
JOIN recipes rc ON rc.id = ar.base_recipe_id
WHERE ar.user_id = $1
ORDER BY ar.created_at DESC
LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("select history: %w", err)
	}
	defer rows.Close()

	items := []ListItem{}
	for rows.Next() {
		var it ListItem
		if err := rows.Scan(&it.ID, &it.BaseRecipeID, &it.Result, &it.Model,
			&it.Adapted, &it.Rating, &it.CreatedAt, &it.BaseTitle); err != nil {
			return nil, fmt.Errorf("scan history item: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (r *Repo) SetRating(ctx context.Context, userID, adaptedID uuid.UUID, rating int16) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE adapted_recipes SET rating = $3 WHERE id = $1 AND user_id = $2`,
		adaptedID, userID, rating)
	if err != nil {
		return fmt.Errorf("update rating: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: adapted recipe %s", httpx.ErrNotFound, adaptedID)
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
