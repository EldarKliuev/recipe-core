// Package cook is the product core: deterministic retrieval of cookable recipes
// from the fridge, orchestration of LLM adaptation jobs, and validation of what
// the model sends back.
package cook

import (
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/catalog"
)

// Match is one retrieval hit. Coverage is expressed as counts plus the actual
// ingredient split, because the UI needs to show what is missing, not just how
// many things are.
type Match struct {
	RecipeID       int64        `json:"recipe_id"`
	Title          string       `json:"title"`
	Category       *string      `json:"category,omitempty"`
	Subcategory    *string      `json:"subcategory,omitempty"`
	Description    *string      `json:"description,omitempty"`
	NumSteps       int          `json:"num_steps"`
	NumIngredients int          `json:"num_ingredients"`
	TotalCount     int          `json:"total_count"`
	MissingCount   int          `json:"missing_count"`
	UseSoonCount   int          `json:"use_soon_count"`
	Have           []Ingredient `json:"have"`
	Missing        []Ingredient `json:"missing"`
}

// Ingredient is the API-facing view of a recipe ingredient line.
type Ingredient struct {
	IngredientID  int64    `json:"ingredient_id"`
	CanonicalName string   `json:"canonical_name"`
	Qty           *float64 `json:"qty,omitempty"`
	Unit          *string  `json:"unit,omitempty"`
}

// MatchesRequest tunes retrieval. There is no cook-time filter: the corpus has
// no such field, so complexity is expressed through num_steps / num_ingredients.
type MatchesRequest struct {
	// MaxMissing is how many ingredients the user is willing to shop for.
	MaxMissing *int `json:"max_missing,omitempty"`
	// MaxSteps and MaxIngredients are the complexity proxies.
	MaxSteps       *int     `json:"max_steps,omitempty"`
	MaxIngredients *int     `json:"max_ingredients,omitempty"`
	Categories     []string `json:"categories,omitempty"`
	Limit          *int     `json:"limit,omitempty"`
}

// MatchesResponse is returned synchronously and never depends on llm-service.
type MatchesResponse struct {
	Items []Match `json:"items"`
	// InventorySize is echoed back so an empty result set is explainable.
	InventorySize int      `json:"inventory_size"`
	Allergens     []string `json:"allergens_applied"`
}

// AdaptRequest asks for LLM adaptation of one of the retrieved recipes.
type AdaptRequest struct {
	// RecipeID must be one of the retrieval hits for this user; the validator
	// enforces that the model answers about this recipe and no other.
	RecipeID int64 `json:"recipe_id"`
	Servings *int  `json:"servings,omitempty"`
	// Notes is free-text user guidance passed through to the model.
	Notes string `json:"notes,omitempty"`
}

// JobStatus is the lifecycle of an adaptation job.
type JobStatus string

const (
	JobQueued  JobStatus = "queued"
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobFailed  JobStatus = "failed"
)

func (s JobStatus) Terminal() bool {
	return s == JobDone || s == JobFailed
}

// Job is the state stored in Redis and streamed over SSE.
type Job struct {
	ID        uuid.UUID `json:"job_id"`
	UserID    uuid.UUID `json:"-"`
	RecipeID  int64     `json:"recipe_id"`
	Status    JobStatus `json:"status"`
	Attempts  int       `json:"attempts"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Result    *Result   `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Result is the validated adaptation handed to the client.
//
// There is no summary or model-authored notes field: llm-service's output schema
// has neither, and a field that is always absent is worse than no field at all.
type Result struct {
	ID           uuid.UUID `json:"id"`
	BaseRecipeID int64     `json:"base_recipe_id"`
	Title        string    `json:"title"`
	// Servings echoes what the user asked for; the model does not report it back.
	Servings *int `json:"servings,omitempty"`
	// Adapted is false when every model attempt was rejected and we fell back to
	// the retrieved recipe as-is.
	Adapted bool `json:"adapted"`
	// FallbackReason explains why Adapted is false.
	FallbackReason string         `json:"fallback_reason,omitempty"`
	Used           []Ingredient   `json:"used_ingredients"`
	Missing        []Ingredient   `json:"missing_ingredients"`
	Substitutions  []Substitution `json:"substitutions"`
	Steps          []string       `json:"steps"`
	Model          string         `json:"model,omitempty"`
	Warnings       []string       `json:"warnings,omitempty"`
	Base           catalog.Recipe `json:"base_recipe"`
}

// Substitution records a swap the model made, e.g. yoghurt for sour cream.
type Substitution struct {
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
	Reason      string `json:"reason,omitempty"`
}
