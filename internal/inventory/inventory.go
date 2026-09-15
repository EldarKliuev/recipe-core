// Package inventory is the user's fridge: which canonical ingredients they have,
// how much, and when it expires.
package inventory

import "time"

// Item is one fridge entry, denormalised with the catalog name for display.
type Item struct {
	IngredientID  int64      `json:"ingredient_id"`
	CanonicalName string     `json:"canonical_name"`
	Category      *string    `json:"category,omitempty"`
	Qty           *float64   `json:"qty,omitempty"`
	Unit          *string    `json:"unit,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	// UseSoon marks items expiring within useSoonWindow, which the ranking
	// boosts and the model is told to prioritise.
	UseSoon bool `json:"use_soon"`
}

// UseSoonWindow is how far ahead an expiry date counts as urgent.
const UseSoonWindow = 3 * 24 * time.Hour
