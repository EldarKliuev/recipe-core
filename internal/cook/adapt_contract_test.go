package cook

import (
	"encoding/json"
	"testing"
)

// The field names below are llm-service's, not ours: they mirror AdaptRequest in
// its app/domain/schemas.py. It declares extra="ignore", so a name we get wrong
// is not reported as an unknown field, it is silently dropped and then rejected
// as missing. This test is the guard against that class of drift.
func TestAdaptRequestMatchesTheLLMServiceSchema(t *testing.T) {
	diet := "vegan"
	servings := 4
	req := LLMAdaptRequest{
		Recipes: []LLMRetrievedRecipe{{
			ID:          1,
			Title:       "Chicken and rice",
			Ingredients: []string{"2 chicken breasts"},
			Steps:       []string{"Cook the rice."},
		}},
		Inventory: []string{"rice"},
		Prefs: LLMPrefs{
			Allergies: []string{"dairy"},
			Diet:      &diet,
			Dislikes:  []string{"olives"},
			Servings:  &servings,
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for _, key := range []string{"recipes", "inventory", "prefs"} {
		if _, ok := got[key]; !ok {
			t.Errorf("payload is missing required field %q: %s", key, body)
		}
	}

	var recipes []map[string]json.RawMessage
	if err := json.Unmarshal(got["recipes"], &recipes); err != nil {
		t.Fatalf("recipes is not a list: %v", err)
	}
	for _, key := range []string{"id", "title", "ingredients", "steps"} {
		if _, ok := recipes[0][key]; !ok {
			t.Errorf("recipe is missing required field %q", key)
		}
	}

	var prefs map[string]json.RawMessage
	if err := json.Unmarshal(got["prefs"], &prefs); err != nil {
		t.Fatalf("prefs is not an object: %v", err)
	}
	for _, key := range []string{"allergies", "diet", "dislikes", "servings"} {
		if _, ok := prefs[key]; !ok {
			t.Errorf("prefs is missing field %q", key)
		}
	}
}

// llm-service is FastAPI, so a JSON null where it declares a list is rejected by
// pydantic before the model is asked anything. Nullable scalars (diet, servings)
// are declared `| None` upstream and may be null; the lists may not.
func TestAdaptRequestNeverSerialisesNullLists(t *testing.T) {
	req := LLMAdaptRequest{
		Recipes: []LLMRetrievedRecipe{{
			ID:          1,
			Ingredients: emptyIfNil[string](nil),
			Steps:       emptyIfNil[string](nil),
		}},
		Inventory: emptyIfNil[string](nil),
		Prefs: LLMPrefs{
			Allergies: emptyIfNil[string](nil),
			Dislikes:  emptyIfNil[string](nil),
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded struct {
		Recipes []struct {
			Ingredients []string `json:"ingredients"`
			Steps       []string `json:"steps"`
		} `json:"recipes"`
		Inventory []string `json:"inventory"`
		Prefs     struct {
			Allergies []string `json:"allergies"`
			Dislikes  []string `json:"dislikes"`
		} `json:"prefs"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	lists := map[string][]string{
		"recipes[0].ingredients": decoded.Recipes[0].Ingredients,
		"recipes[0].steps":       decoded.Recipes[0].Steps,
		"inventory":              decoded.Inventory,
		"prefs.allergies":        decoded.Prefs.Allergies,
		"prefs.dislikes":         decoded.Prefs.Dislikes,
	}
	for field, list := range lists {
		if list == nil {
			t.Errorf("%s serialised as null: %s", field, body)
		}
	}
}

func TestEmptyIfNilLeavesPopulatedSlicesAlone(t *testing.T) {
	in := []string{"dairy"}
	if got := emptyIfNil(in); len(got) != 1 || got[0] != "dairy" {
		t.Errorf("emptyIfNil(%v) = %v, want the input unchanged", in, got)
	}
	if got := emptyIfNil[string](nil); got == nil || len(got) != 0 {
		t.Errorf("emptyIfNil(nil) = %v, want an empty non-nil slice", got)
	}
}

// The response side of the same contract: a body shaped like llm-service's
// AdaptResponse must fill every field. A misspelt tag would not fail decoding,
// it would leave the field empty, and for step_minutes that silently means no
// timers at all.
func TestAdaptResponseDecodesTheLLMServiceSchema(t *testing.T) {
	body := `{
		"recipe": {
			"base_recipe_id": 7,
			"title": "Rice bowl",
			"steps": ["Rinse the rice.", "Simmer the rice."],
			"step_minutes": [null, 18],
			"used_ingredients": ["rice"],
			"missing_ingredients": [],
			"substitutions": [{"missing": "butter", "replace_with": "oil", "note": "dairy-free"}]
		},
		"meta": {"provider": "lmstudio", "model": "qwen", "prompt_version": "v2"}
	}`
	var got LLMAdaptResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	r := got.Recipe
	if r.BaseRecipeID != 7 || r.Title != "Rice bowl" || len(r.Steps) != 2 ||
		len(r.UsedIngredients) != 1 || len(r.Substitutions) != 1 {
		t.Errorf("Recipe = %+v, want every field filled", r)
	}
	if len(r.StepMinutes) != 2 || r.StepMinutes[0] != nil || r.StepMinutes[1] == nil || *r.StepMinutes[1] != 18 {
		t.Errorf("StepMinutes = %v, want [null 18]", r.StepMinutes)
	}
}
