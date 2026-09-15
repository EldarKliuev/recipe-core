// Package user owns the profile and the preferences that constrain retrieval
// and adaptation: allergens, diets, disliked ingredients, cuisines.
package user

import (
	"fmt"
	"slices"
	"strings"

	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

// Prefs are user constraints. Allergens are a hard filter enforced in SQL and in
// the validator; diets and cuisines are soft signals passed to the model.
type Prefs struct {
	Allergens             []string `json:"allergens"`
	Diets                 []string `json:"diets"`
	DislikedIngredientIDs []int64  `json:"disliked_ingredient_ids"`
	Cuisines              []string `json:"cuisines"`
}

// KnownAllergens is the closed vocabulary of allergen tags. It is closed on
// purpose: an unrecognised tag in prefs would silently filter nothing, which is
// the dangerous failure direction for an allergy.
var KnownAllergens = []string{
	"dairy", "eggs", "fish", "shellfish", "tree_nuts", "peanuts",
	"wheat", "gluten", "soy", "sesame", "mustard", "celery", "sulfites",
}

// KnownDiets is likewise a closed vocabulary, kept in sync with the web client.
var KnownDiets = []string{
	"vegetarian", "vegan", "pescatarian", "halal", "kosher",
	"keto", "paleo", "low_carb", "low_sodium", "gluten_free", "dairy_free",
}

const (
	maxListLen = 40
	maxTagLen  = 40
)

// Validate normalises tag casing and rejects unknown allergens and diets.
func (p *Prefs) Validate() error {
	var err error
	if p.Allergens, err = normaliseTags("allergens", p.Allergens, KnownAllergens); err != nil {
		return err
	}
	if p.Diets, err = normaliseTags("diets", p.Diets, KnownDiets); err != nil {
		return err
	}
	if p.Cuisines, err = normaliseTags("cuisines", p.Cuisines, nil); err != nil {
		return err
	}
	if len(p.DislikedIngredientIDs) > maxListLen {
		return httpx.Invalidf("disliked_ingredient_ids must contain at most %d entries", maxListLen)
	}
	slices.Sort(p.DislikedIngredientIDs)
	p.DislikedIngredientIDs = slices.Compact(p.DislikedIngredientIDs)
	for _, id := range p.DislikedIngredientIDs {
		if id <= 0 {
			return httpx.Invalidf("disliked_ingredient_ids must contain positive ids")
		}
	}
	if p.Allergens == nil {
		p.Allergens = []string{}
	}
	if p.Diets == nil {
		p.Diets = []string{}
	}
	if p.Cuisines == nil {
		p.Cuisines = []string{}
	}
	if p.DislikedIngredientIDs == nil {
		p.DislikedIngredientIDs = []int64{}
	}
	return nil
}

// normaliseTags lowercases, trims, de-duplicates and, when allowed is non-nil,
// rejects values outside the vocabulary.
func normaliseTags(field string, values, allowed []string) ([]string, error) {
	if len(values) > maxListLen {
		return nil, httpx.Invalidf("%s must contain at most %d entries", field, maxListLen)
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if len(v) > maxTagLen {
			return nil, httpx.Invalidf("%s entries must be at most %d characters", field, maxTagLen)
		}
		if allowed != nil && !slices.Contains(allowed, v) {
			return nil, httpx.Invalidf("%s contains unsupported value %q; allowed: %s",
				field, v, strings.Join(allowed, ", "))
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out, nil
}

// EffectiveAllergens returns the deny-list to apply, expanding diets that imply
// allergen exclusions so that "gluten_free" cannot be defeated by forgetting to
// also tick "gluten".
func (p Prefs) EffectiveAllergens() []string {
	out := slices.Clone(p.Allergens)
	add := func(tag string) {
		if !slices.Contains(out, tag) {
			out = append(out, tag)
		}
	}
	for _, diet := range p.Diets {
		switch diet {
		case "gluten_free":
			add("gluten")
			add("wheat")
		case "dairy_free":
			add("dairy")
		}
	}
	slices.Sort(out)
	return out
}

func (p Prefs) String() string {
	return fmt.Sprintf("allergens=%v diets=%v cuisines=%v disliked=%d",
		p.Allergens, p.Diets, p.Cuisines, len(p.DislikedIngredientIDs))
}
