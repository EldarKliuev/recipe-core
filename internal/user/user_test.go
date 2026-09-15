package user

import (
	"errors"
	"slices"
	"testing"

	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

func TestValidateNormalisesTags(t *testing.T) {
	p := Prefs{
		Allergens: []string{" Dairy ", "PEANUTS", "dairy"},
		Diets:     []string{"Vegan"},
		Cuisines:  []string{"Thai", "thai", ""},
	}

	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !slices.Equal(p.Allergens, []string{"dairy", "peanuts"}) {
		t.Errorf("Allergens = %v, want lowercased and de-duplicated", p.Allergens)
	}
	if !slices.Equal(p.Cuisines, []string{"thai"}) {
		t.Errorf("Cuisines = %v, want [thai]", p.Cuisines)
	}
	// Empty slices rather than nil, so the JSON response has [] not null.
	if p.DislikedIngredientIDs == nil {
		t.Error("DislikedIngredientIDs = nil, want an empty slice for JSON output")
	}
}

// An unknown allergen tag would filter nothing, which is the dangerous direction
// for an allergy, so the vocabulary is closed.
func TestValidateRejectsUnknownAllergenAndDiet(t *testing.T) {
	p := Prefs{Allergens: []string{"nightshades"}}
	if err := p.Validate(); !errors.Is(err, httpx.ErrInvalidInput) {
		t.Errorf("unknown allergen error = %v, want ErrInvalidInput", err)
	}

	p = Prefs{Diets: []string{"carnivore-adjacent"}}
	if err := p.Validate(); !errors.Is(err, httpx.ErrInvalidInput) {
		t.Errorf("unknown diet error = %v, want ErrInvalidInput", err)
	}
}

func TestValidateRejectsNonPositiveIngredientIDs(t *testing.T) {
	p := Prefs{DislikedIngredientIDs: []int64{5, 0}}
	if err := p.Validate(); !errors.Is(err, httpx.ErrInvalidInput) {
		t.Errorf("error = %v, want ErrInvalidInput", err)
	}
}

// A user who ticks "gluten_free" but forgets the "gluten" allergen must still get
// gluten excluded, so diets widen the hard deny-list.
func TestEffectiveAllergensExpandsDiets(t *testing.T) {
	p := Prefs{Allergens: []string{"peanuts"}, Diets: []string{"gluten_free", "dairy_free"}}

	got := p.EffectiveAllergens()
	for _, want := range []string{"peanuts", "gluten", "wheat", "dairy"} {
		if !slices.Contains(got, want) {
			t.Errorf("EffectiveAllergens() = %v, missing %q", got, want)
		}
	}
}

func TestEffectiveAllergensDoesNotMutateThePrefs(t *testing.T) {
	p := Prefs{Allergens: []string{"peanuts"}, Diets: []string{"dairy_free"}}

	_ = p.EffectiveAllergens()
	if !slices.Equal(p.Allergens, []string{"peanuts"}) {
		t.Errorf("Allergens = %v after EffectiveAllergens(), want it left alone", p.Allergens)
	}
}
