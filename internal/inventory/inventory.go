// Package inventory is the user's fridge: which canonical ingredients they have,
// how much, and when it expires.
package inventory

import (
	"encoding/json"
	"time"
)

// Item is one fridge entry, denormalised with the catalog name for display.
type Item struct {
	IngredientID  int64    `json:"ingredient_id"`
	CanonicalName string   `json:"canonical_name"`
	Category      *string  `json:"category,omitempty"`
	Qty           *float64 `json:"qty,omitempty"`
	Unit          *string  `json:"unit,omitempty"`
	// ExpiresAt is a calendar date; MarshalJSON writes it as YYYY-MM-DD.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// UseSoon marks items expiring within useSoonWindow, which the ranking
	// boosts and the model is told to prioritise.
	UseSoon bool `json:"use_soon"`
}

// MarshalJSON writes expires_at as a date without time, matching `format: date`
// in api/openapi.yaml and the YYYY-MM-DD the add endpoint accepts. The default
// time.Time encoding would add midnight UTC, which a client in another time
// zone can shift to the previous day.
func (it Item) MarshalJSON() ([]byte, error) {
	type plain Item // drops this method so json.Marshal does not recurse
	out := struct {
		plain
		ExpiresAt *string `json:"expires_at,omitempty"` // shadows plain.ExpiresAt
	}{plain: plain(it)}
	if it.ExpiresAt != nil {
		date := it.ExpiresAt.Format(time.DateOnly)
		out.ExpiresAt = &date
	}
	return json.Marshal(out)
}

// UseSoonWindow is how far ahead an expiry date counts as urgent.
const UseSoonWindow = 3 * 24 * time.Hour
