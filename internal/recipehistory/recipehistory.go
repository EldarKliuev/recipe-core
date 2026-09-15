// Package recipehistory persists adaptation outcomes: the adapted recipes a user
// has produced, their ratings, and one telemetry row per model call.
package recipehistory

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Entry is a stored adaptation. Result is kept opaque here so this package does
// not depend on the cook domain types, which would be a circular dependency.
//
// The type is not called Adapted because it carries a bool field of that name,
// and an embedded struct whose name shadows its own field is a trap: selector
// expressions silently resolve to the struct instead of the flag.
type Entry struct {
	ID           uuid.UUID       `json:"id"`
	UserID       uuid.UUID       `json:"-"`
	BaseRecipeID int64           `json:"base_recipe_id"`
	Result       json.RawMessage `json:"result"`
	Model        *string         `json:"model,omitempty"`
	Adapted      bool            `json:"adapted"`
	Rating       *int16          `json:"rating,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Run is one attempt at calling llm-service, successful or not. Rejected attempts
// are recorded too, so the validator rejection rate is measurable.
type Run struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	JobID         uuid.UUID
	Model         string
	PromptVersion string
	LatencyMS     int
	PromptTokens  int
	OutputTokens  int
	Retries       int
	// Status is one of: success, rejected, error.
	Status string
	Error  string
}

const (
	RunSuccess  = "success"
	RunRejected = "rejected"
	RunError    = "error"
)
