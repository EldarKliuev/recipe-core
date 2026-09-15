// Package catalog is read-only access to the recipe corpus and the canonical
// ingredient catalog. It is the only place that resolves free-text ingredient
// names to canonical ids.
package catalog

import "time"

// Ingredient is a canonical catalog entry.
type Ingredient struct {
	ID            int64    `json:"id"`
	CanonicalName string   `json:"canonical_name"`
	Category      *string  `json:"category,omitempty"`
	Allergens     []string `json:"allergens,omitempty"`
	IsStaple      bool     `json:"is_staple"`
}

// Recipe is corpus metadata. Note the absence of directions: verbatim steps are
// an internal reference and are never serialised to API clients.
type Recipe struct {
	ID             int64     `json:"id"`
	Title          string    `json:"title"`
	Category       *string   `json:"category,omitempty"`
	Subcategory    *string   `json:"subcategory,omitempty"`
	Description    *string   `json:"description,omitempty"`
	NumIngredients int       `json:"num_ingredients"`
	NumSteps       int       `json:"num_steps"`
	Source         *string   `json:"source,omitempty"`
	CreatedAt      time.Time `json:"-"`
}

// RecipeIngredient links a recipe to a canonical ingredient.
type RecipeIngredient struct {
	IngredientID  int64    `json:"ingredient_id"`
	CanonicalName string   `json:"canonical_name"`
	RawText       string   `json:"raw_text"`
	Qty           *float64 `json:"qty,omitempty"`
	Unit          *string  `json:"unit,omitempty"`
}

// Resolution is the outcome of matching one free-text ingredient name against
// the catalog.
type Resolution struct {
	Input      string
	Ingredient Ingredient
	// Exact is false when the match came from trigram similarity rather than a
	// canonical name or a registered synonym.
	Exact bool
}
