package cook

import (
	"context"
	"strings"
	"testing"

	"github.com/recipe-ai/core-api/internal/catalog"
)

// fakeCatalog resolves names from a fixed table, standing in for Postgres.
type fakeCatalog struct {
	byName    map[string]catalog.Ingredient
	allergens map[int64][]string
}

func (f *fakeCatalog) ResolveNames(_ context.Context, names []string) ([]catalog.Resolution, []string, error) {
	var resolved []catalog.Resolution
	var unresolved []string
	for _, name := range names {
		ing, ok := f.byName[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			unresolved = append(unresolved, name)
			continue
		}
		resolved = append(resolved, catalog.Resolution{Input: name, Ingredient: ing, Exact: true})
	}
	return resolved, unresolved, nil
}

func (f *fakeCatalog) AllergensForIngredients(_ context.Context, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	for _, id := range ids {
		if tags, ok := f.allergens[id]; ok {
			out[id] = tags
		}
	}
	return out, nil
}

func testValidator() (*Validator, ValidationInput) {
	cat := &fakeCatalog{
		byName: map[string]catalog.Ingredient{
			"chicken breast": {ID: 1, CanonicalName: "chicken breast"},
			"rice":           {ID: 2, CanonicalName: "rice"},
			"soy sauce":      {ID: 3, CanonicalName: "soy sauce"},
			"butter":         {ID: 4, CanonicalName: "butter"},
			"salt":           {ID: 5, CanonicalName: "salt", IsStaple: true},
		},
		allergens: map[int64][]string{
			3: {"soy", "wheat", "gluten"},
			4: {"dairy"},
		},
	}
	cups := "cup"
	two := 2.0
	in := ValidationInput{
		CandidateIDs: []int64{100, 101},
		HaveIDs:      []int64{1, 2},
		Bases: map[int64]catalog.Recipe{
			100: {ID: 100, Title: "Chicken and rice"},
			101: {ID: 101, Title: "Fried rice"},
		},
		// The model returns bare names, so amounts are taken from these corpus
		// rows rather than from anything it said.
		BaseLines: map[int64][]catalog.RecipeIngredient{
			100: {
				{IngredientID: 1, CanonicalName: "chicken breast"},
				{IngredientID: 2, CanonicalName: "rice", Qty: &two, Unit: &cups},
			},
		},
	}
	return NewValidator(cat), in
}

func TestValidateRecomputesMissingFromTheCatalog(t *testing.T) {
	v, in := testValidator()

	// The model claims soy sauce is on hand. It is not in HaveIDs, so the
	// validator must move it into missing regardless of what the model said.
	out := LLMAdaptedRecipe{
		BaseRecipeID: 100,
		Title:        "Chicken rice bowl",
		UsedIngredients: []string{
			"chicken breast", "rice", "soy sauce", "salt",
		},
		Steps: []string{"Cook the rice.", "Sear the chicken."},
	}

	got, err := v.Validate(context.Background(), in, out)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !got.Adapted {
		t.Error("Adapted = false, want true for a valid answer")
	}
	if names := names(got.Used); !equal(names, []string{"chicken breast", "rice", "salt"}) {
		t.Errorf("Used = %v, want chicken breast, rice and the staple salt", names)
	}
	if names := names(got.Missing); !equal(names, []string{"soy sauce"}) {
		t.Errorf("Missing = %v, want soy sauce moved out of used", names)
	}
}

// llm-service returns ingredient names with no amounts, so quantities must come
// from the corpus row for the chosen recipe.
func TestValidateTakesAmountsFromTheCorpusNotTheModel(t *testing.T) {
	v, in := testValidator()

	out := LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice", "chicken breast"},
		Steps:           []string{"Cook the rice."},
	}

	got, err := v.Validate(context.Background(), in, out)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	var rice *Ingredient
	for i := range got.Used {
		if got.Used[i].CanonicalName == "rice" {
			rice = &got.Used[i]
		}
	}
	if rice == nil {
		t.Fatalf("Used = %v, want rice among them", names(got.Used))
	}
	if rice.Qty == nil || *rice.Qty != 2 {
		t.Errorf("rice qty = %v, want 2 from the corpus row", rice.Qty)
	}
	if rice.Unit == nil || *rice.Unit != "cup" {
		t.Errorf("rice unit = %v, want cup from the corpus row", rice.Unit)
	}
}

// Servings is echoed from the request, because llm-service does not report it.
func TestValidateEchoesRequestedServings(t *testing.T) {
	v, in := testValidator()
	four := 4
	in.Servings = &four

	got, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Steps:           []string{"Cook the rice."},
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got.Servings == nil || *got.Servings != 4 {
		t.Errorf("Servings = %v, want the requested 4", got.Servings)
	}
}

func TestValidateRejectsRecipeOutsideTheRetrievalSet(t *testing.T) {
	v, in := testValidator()

	out := LLMAdaptedRecipe{
		BaseRecipeID:    999,
		UsedIngredients: []string{"rice"},
		Steps:           []string{"Cook the rice."},
	}

	_, err := v.Validate(context.Background(), in, out)
	rej, ok := AsRejection(err)
	if !ok {
		t.Fatalf("Validate() error = %v, want a rejection", err)
	}
	if rej.Reason != RejectWrongRecipe {
		t.Errorf("reason = %q, want %q", rej.Reason, RejectWrongRecipe)
	}
}

func TestValidateRejectsDeniedAllergenRatherThanWarning(t *testing.T) {
	v, in := testValidator()
	in.DenyAllergens = []string{"dairy"}

	out := LLMAdaptedRecipe{
		BaseRecipeID:       100,
		UsedIngredients:    []string{"rice"},
		MissingIngredients: []string{"butter"},
		Steps:              []string{"Melt the butter."},
	}

	_, err := v.Validate(context.Background(), in, out)
	rej, ok := AsRejection(err)
	if !ok {
		t.Fatalf("Validate() error = %v, want a rejection", err)
	}
	if rej.Reason != RejectAllergen {
		t.Errorf("reason = %q, want %q", rej.Reason, RejectAllergen)
	}
	if !strings.Contains(rej.Detail, "butter") {
		t.Errorf("detail = %q, want it to name the offending ingredient", rej.Detail)
	}
}

// An allergen appearing only in the missing list is still a violation: the user
// would be told to go buy it.
func TestValidateRejectsAllergenEvenWhenOnlyListedAsMissing(t *testing.T) {
	v, in := testValidator()
	in.DenyAllergens = []string{"soy"}

	out := LLMAdaptedRecipe{
		BaseRecipeID:       101,
		UsedIngredients:    []string{"rice"},
		MissingIngredients: []string{"soy sauce"},
		Steps:              []string{"Add soy sauce."},
	}

	if _, err := v.Validate(context.Background(), in, out); err == nil {
		t.Fatal("Validate() = nil error, want an allergen rejection")
	}
}

// A replacement is advice the user may act on, so a denied allergen in it rejects
// the answer even though it appears in neither ingredient list.
func TestValidateRejectsDeniedAllergenInSubstitutionReplacement(t *testing.T) {
	v, in := testValidator()
	in.DenyAllergens = []string{"dairy"}

	out := LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Substitutions: []LLMSubstitution{
			{Missing: "soy sauce", ReplaceWith: "  Butter ", Note: "richer"},
		},
		Steps: []string{"Cook the rice."},
	}

	_, err := v.Validate(context.Background(), in, out)
	rej, ok := AsRejection(err)
	if !ok {
		t.Fatalf("Validate() error = %v, want a rejection", err)
	}
	if rej.Reason != RejectAllergen {
		t.Errorf("reason = %q, want %q", rej.Reason, RejectAllergen)
	}
	if !strings.Contains(rej.Detail, "butter") {
		t.Errorf("detail = %q, want it to name the replacement", rej.Detail)
	}
}

// Swapping a denied ingredient for a safe one is the behaviour we want, so only
// the replacement is checked, never the ingredient being replaced.
func TestValidateAllowsSubstitutionThatRemovesADeniedAllergen(t *testing.T) {
	v, in := testValidator()
	in.DenyAllergens = []string{"dairy"}

	out := LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Substitutions: []LLMSubstitution{
			{Missing: "butter", ReplaceWith: "chicken breast", Note: "no dairy"},
		},
		Steps: []string{"Cook the rice."},
	}

	got, err := v.Validate(context.Background(), in, out)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	want := Substitution{Original: "butter", Replacement: "chicken breast", Reason: "no dairy"}
	if len(got.Substitutions) != 1 || got.Substitutions[0] != want {
		t.Errorf("Substitutions = %+v, want only %+v", got.Substitutions, want)
	}
	// A replacement is advice, not a recipe ingredient: the used and missing
	// lists are still built only from what the model listed there.
	if names := names(got.Used); !equal(names, []string{"rice"}) {
		t.Errorf("Used = %v, want only rice", names)
	}
	if len(got.Missing) != 0 {
		t.Errorf("Missing = %v, want none", names(got.Missing))
	}
}

// An incomplete substitution never reaches the user, so it must not cost the
// whole answer a rejection.
func TestValidateIgnoresDeniedAllergenInADroppedSubstitution(t *testing.T) {
	v, in := testValidator()
	in.DenyAllergens = []string{"dairy"}

	out := LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Substitutions:   []LLMSubstitution{{Missing: "  ", ReplaceWith: "butter"}},
		Steps:           []string{"Cook the rice."},
	}

	got, err := v.Validate(context.Background(), in, out)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(got.Substitutions) != 0 {
		t.Errorf("Substitutions = %+v, want the incomplete one dropped", got.Substitutions)
	}
}

// A replacement outside the catalog cannot be verified, so it is reported the same
// way as any other unknown name.
func TestValidateReportsSubstitutionReplacementAbsentFromTheCatalog(t *testing.T) {
	v, in := testValidator()

	out := LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Substitutions:   []LLMSubstitution{{Missing: "soy sauce", ReplaceWith: "unobtainium powder"}},
		Steps:           []string{"Cook the rice."},
	}

	got, err := v.Validate(context.Background(), in, out)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "unobtainium powder") {
		t.Errorf("Warnings = %v, want one warning naming the unknown replacement", got.Warnings)
	}
}

func TestValidateRejectsEmptySteps(t *testing.T) {
	v, in := testValidator()

	out := LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Steps:           []string{"  ", ""},
	}

	_, err := v.Validate(context.Background(), in, out)
	rej, ok := AsRejection(err)
	if !ok {
		t.Fatalf("Validate() error = %v, want a rejection", err)
	}
	if rej.Reason != RejectNoSteps {
		t.Errorf("reason = %q, want %q", rej.Reason, RejectNoSteps)
	}
}

func TestValidateReportsIngredientsAbsentFromTheCatalog(t *testing.T) {
	v, in := testValidator()

	out := LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice", "unobtainium powder"},
		Steps:           []string{"Cook the rice."},
	}

	got, err := v.Validate(context.Background(), in, out)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "unobtainium powder") {
		t.Errorf("Warnings = %v, want one warning naming the unknown ingredient", got.Warnings)
	}
	if names := names(got.Used); !equal(names, []string{"rice"}) {
		t.Errorf("Used = %v, want only the ingredient that resolved", names)
	}
}

// A name outside the catalog has no id, so its allergens cannot be checked. With a
// deny-list in force that is a rejection, not a warning.
func TestValidateRejectsUnresolvedNameWhenAllergensAreDenied(t *testing.T) {
	cases := map[string]LLMAdaptedRecipe{
		"in used": {
			BaseRecipeID:    100,
			UsedIngredients: []string{"rice", "shrimp paste"},
			Steps:           []string{"Cook the rice."},
		},
		"in missing": {
			BaseRecipeID:       100,
			UsedIngredients:    []string{"rice"},
			MissingIngredients: []string{"shrimp paste"},
			Steps:              []string{"Cook the rice."},
		},
		"in a substitution replacement": {
			BaseRecipeID:    100,
			UsedIngredients: []string{"rice"},
			Substitutions:   []LLMSubstitution{{Missing: "soy sauce", ReplaceWith: "shrimp paste"}},
			Steps:           []string{"Cook the rice."},
		},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			v, in := testValidator()
			in.DenyAllergens = []string{"shellfish"}

			_, err := v.Validate(context.Background(), in, out)
			rej, ok := AsRejection(err)
			if !ok {
				t.Fatalf("Validate() error = %v, want a rejection", err)
			}
			if rej.Reason != RejectUnverifiable {
				t.Errorf("reason = %q, want %q", rej.Reason, RejectUnverifiable)
			}
			if !strings.Contains(rej.Detail, "shrimp paste") {
				t.Errorf("detail = %q, want it to name the unknown ingredient", rej.Detail)
			}
		})
	}
}

// The name being replaced is not advice, so it not resolving must not cost the
// answer a rejection: it is the replacement that the user would act on.
func TestValidateAcceptsUnresolvedSubstitutionOriginalWhenAllergensAreDenied(t *testing.T) {
	v, in := testValidator()
	in.DenyAllergens = []string{"dairy"}

	got, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Substitutions:   []LLMSubstitution{{Missing: "feta", ReplaceWith: "chicken breast"}},
		Steps:           []string{"Cook the rice."},
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none for the name being replaced", got.Warnings)
	}
}

// A base line the steps use is still something the user lacks, even if the model
// forgot to list it.
func TestValidateAddsBaseLinesTheStepsUseToMissing(t *testing.T) {
	v, in := testValidator()
	cup := "cup"
	three := 3.0
	in.BaseLines[100] = append(in.BaseLines[100],
		catalog.RecipeIngredient{IngredientID: 3, CanonicalName: "soy sauce", Qty: &three, Unit: &cup})

	got, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Steps:           []string{"Cook the rice.", "Sear the chicken breast with soy sauce."},
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(got.Missing) != 1 || got.Missing[0].CanonicalName != "soy sauce" {
		t.Fatalf("Missing = %v, want only soy sauce", names(got.Missing))
	}
	if got.Missing[0].Qty == nil || *got.Missing[0].Qty != 3 || got.Missing[0].Unit == nil || *got.Missing[0].Unit != "cup" {
		t.Errorf("soy sauce = %+v, want the corpus amount", got.Missing[0])
	}
	// The steps use chicken breast too, but the user has it, and only what the model
	// named is reported as used.
	if names := names(got.Used); !equal(names, []string{"rice"}) {
		t.Errorf("Used = %v, want only rice", names)
	}
}

// A line the model named as missing appears once, not again as a base line.
func TestValidateDoesNotDuplicateBaseLinesTheModelListed(t *testing.T) {
	v, in := testValidator()
	in.BaseLines[100] = append(in.BaseLines[100],
		catalog.RecipeIngredient{IngredientID: 3, CanonicalName: "soy sauce"})

	got, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
		BaseRecipeID:       100,
		MissingIngredients: []string{"soy sauce"},
		Steps:              []string{"Add the soy sauce."},
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if names := names(got.Missing); !equal(names, []string{"soy sauce"}) {
		t.Errorf("Missing = %v, want soy sauce exactly once", names)
	}
}

// The model often drops a line without recording a substitution. Asking the user
// to buy something the steps never use would be wrong.
func TestValidateLeavesBaseLinesTheStepsDoNotUseOutOfMissing(t *testing.T) {
	cases := map[string]struct {
		line  string
		steps string
		want  bool
	}{
		"absent":                  {"salmon fillet", "Cook the rice.", false},
		"only one word of a name": {"salmon fillet", "Sear the chicken fillet.", false},
		"inside a longer word":    {"lime", "Add the slime.", false},
		"every word present":      {"salmon fillet", "Sear the salmon fillet.", true},
		"different case":          {"salmon fillet", "SALMON FILLET goes in first.", true},
		"plural in the steps":     {"tomato", "Chop the tomatoes.", true},
		"plural in the catalog":   {"kidney beans", "Add the kidney bean.", true},
		"next to punctuation":     {"lime", "Squeeze (one lime), then serve.", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v, in := testValidator()
			in.BaseLines[100] = append(in.BaseLines[100],
				catalog.RecipeIngredient{IngredientID: 9, CanonicalName: tc.line})

			got, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
				BaseRecipeID:    100,
				UsedIngredients: []string{"rice"},
				Steps:           []string{tc.steps},
			})
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if added := len(got.Missing) == 1; added != tc.want {
				t.Errorf("Missing = %v, want line added = %v", names(got.Missing), tc.want)
			}
		})
	}
}

// Telling the user to buy what the model just swapped out would be wrong.
func TestValidateLeavesReplacedBaseLinesOutOfMissing(t *testing.T) {
	v, in := testValidator()
	in.BaseLines[100] = append(in.BaseLines[100],
		catalog.RecipeIngredient{IngredientID: 3, CanonicalName: "soy sauce"})

	got, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Substitutions:   []LLMSubstitution{{Missing: "Soy Sauce", ReplaceWith: "salt"}},
		Steps:           []string{"Use salt instead of the soy sauce."},
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(got.Missing) != 0 {
		t.Errorf("Missing = %v, want the replaced soy sauce left out", names(got.Missing))
	}
}

// Only the base recipe the model chose counts, not the other candidates.
func TestValidateIgnoresBaseLinesOfOtherCandidates(t *testing.T) {
	v, in := testValidator()
	in.BaseLines[101] = []catalog.RecipeIngredient{{IngredientID: 4, CanonicalName: "butter"}}

	got, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Steps:           []string{"Melt the butter over the rice."},
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(got.Missing) != 0 {
		t.Errorf("Missing = %v, want nothing from another candidate", names(got.Missing))
	}
}

// A denied allergen in a base line rejects the answer whether or not the steps
// mention it: retrieval should have kept the recipe out, and word matching decides
// only the shopping list, never the allergen check.
func TestValidateRejectsDeniedAllergenInAnUnmentionedBaseLine(t *testing.T) {
	v, in := testValidator()
	in.DenyAllergens = []string{"dairy"}
	in.BaseLines[100] = append(in.BaseLines[100],
		catalog.RecipeIngredient{IngredientID: 4, CanonicalName: "butter"})

	_, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Steps:           []string{"Cook the rice."},
	})
	rej, ok := AsRejection(err)
	if !ok {
		t.Fatalf("Validate() error = %v, want a rejection", err)
	}
	if rej.Reason != RejectAllergen || !strings.Contains(rej.Detail, "butter") {
		t.Errorf("rejection = %+v, want an allergen rejection naming butter", rej)
	}
}

// Replacing the base line is how the model removes the allergen, so that line no
// longer counts against the answer.
func TestValidateAcceptsDeniedBaseLineThatWasReplaced(t *testing.T) {
	v, in := testValidator()
	in.DenyAllergens = []string{"dairy"}
	in.BaseLines[100] = append(in.BaseLines[100],
		catalog.RecipeIngredient{IngredientID: 4, CanonicalName: "butter"})

	got, err := v.Validate(context.Background(), in, LLMAdaptedRecipe{
		BaseRecipeID:    100,
		UsedIngredients: []string{"rice"},
		Substitutions:   []LLMSubstitution{{Missing: "butter", ReplaceWith: "salt"}},
		Steps:           []string{"Cook the rice."},
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(got.Missing) != 0 {
		t.Errorf("Missing = %v, want the replaced butter left out", names(got.Missing))
	}
}

func TestValidateFallsBackToCorpusTitle(t *testing.T) {
	v, in := testValidator()

	out := LLMAdaptedRecipe{
		BaseRecipeID:    100,
		Title:           "   ",
		UsedIngredients: []string{"rice"},
		Steps:           []string{"Cook the rice."},
	}

	got, err := v.Validate(context.Background(), in, out)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got.Title != "Chicken and rice" {
		t.Errorf("Title = %q, want the corpus title", got.Title)
	}
}

func names(items []Ingredient) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.CanonicalName)
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}
