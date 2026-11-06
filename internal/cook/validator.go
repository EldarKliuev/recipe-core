package cook

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/recipe-ai/core-api/internal/catalog"
)

// RejectionReason classifies why a model answer was thrown away. It is recorded
// per attempt in generation_runs so the rejection rate is measurable.
type RejectionReason string

const (
	RejectWrongRecipe   RejectionReason = "base_recipe_mismatch"
	RejectAllergen      RejectionReason = "allergen_violation"
	RejectNoSteps       RejectionReason = "empty_steps"
	RejectUnknownRecipe RejectionReason = "unknown_base_recipe"
	RejectUnverifiable  RejectionReason = "unverifiable_ingredient"
)

// Rejection is returned when the model output is semantically invalid. It is a
// hard failure: the answer is discarded and the attempt is retried, never
// downgraded to a warning in the text shown to the user.
type Rejection struct {
	Reason RejectionReason
	Detail string
}

func (r *Rejection) Error() string {
	return fmt.Sprintf("model output rejected (%s): %s", r.Reason, r.Detail)
}

// AsRejection reports whether err is a validator rejection.
func AsRejection(err error) (*Rejection, bool) {
	var rej *Rejection
	ok := errors.As(err, &rej)
	return rej, ok
}

// nameResolver is the catalog surface the validator needs: it maps the model's
// free-text ingredient names back onto canonical ids so that allergen and
// inventory checks work on ids, not strings.
type nameResolver interface {
	ResolveNames(ctx context.Context, names []string) ([]catalog.Resolution, []string, error)
	AllergensForIngredients(ctx context.Context, ids []int64) (map[int64][]string, error)
}

// Validator turns a schema-valid model answer into a semantically trustworthy
// Result, or rejects it.
type Validator struct {
	catalog nameResolver
}

func NewValidator(c nameResolver) *Validator {
	return &Validator{catalog: c}
}

// ValidationInput is everything the validator needs to judge one answer.
type ValidationInput struct {
	// CandidateIDs is the retrieval set that was sent to the model. An answer
	// about anything else is a rejection.
	CandidateIDs []int64
	// HaveIDs is the fridge plus staples.
	HaveIDs []int64
	// DenyAllergens is the user's effective allergen deny-list.
	DenyAllergens []string
	Bases         map[int64]catalog.Recipe
	// BaseLines are the corpus ingredient rows per candidate recipe. The model
	// returns bare names, so amounts in the result come from here.
	BaseLines map[int64][]catalog.RecipeIngredient
	// Servings is what the user asked for. llm-service does not echo it back, so
	// the result reports the request rather than a guess.
	Servings *int
}

// Validate enforces the semantic contract:
//
//   - base_recipe_id must be one of the candidates we offered;
//   - every ingredient is normalised against the catalog, and anything not in
//     the fridge or staples is moved into missing[] regardless of which bucket
//     the model put it in;
//   - lines of the base recipe the user lacks are added to missing[] when the steps
//     mention them, even if the model did not list them, unless it replaced them;
//   - any ingredient or substitution replacement carrying a denied allergen tag
//     rejects the whole answer, and so does a name outside the catalog while a
//     deny-list is in force, because its allergens cannot be checked;
//   - an answer with no steps is useless and is rejected.
func (v *Validator) Validate(ctx context.Context, in ValidationInput, out LLMAdaptedRecipe) (Result, error) {
	if !slices.Contains(in.CandidateIDs, out.BaseRecipeID) {
		return Result{}, &Rejection{
			Reason: RejectWrongRecipe,
			Detail: fmt.Sprintf("base_recipe_id %d was not in the retrieval set %v", out.BaseRecipeID, in.CandidateIDs),
		}
	}
	base, ok := in.Bases[out.BaseRecipeID]
	if !ok {
		return Result{}, &Rejection{
			Reason: RejectUnknownRecipe,
			Detail: fmt.Sprintf("recipe %d is not in the corpus", out.BaseRecipeID),
		}
	}
	if len(nonEmpty(out.Steps)) == 0 {
		return Result{}, &Rejection{Reason: RejectNoSteps, Detail: "model returned no steps"}
	}

	// The model's own used/missing split is advisory. We recompute it from the
	// catalog, because the model has no reliable view of the fridge.
	claimed := append(slices.Clone(out.UsedIngredients), out.MissingIngredients...)
	names := make([]string, 0, len(claimed))
	for _, item := range claimed {
		if name := strings.TrimSpace(item); name != "" {
			names = append(names, name)
		}
	}

	// A replacement is advice the user may act on, so it is checked like any other
	// ingredient. The ingredient being replaced is not: swapping an allergen out is
	// exactly what the user needs. Incomplete pairs are never shown, so they are
	// dropped here and cannot reject the answer.
	substitutions := make([]LLMSubstitution, 0, len(out.Substitutions))
	for _, sub := range out.Substitutions {
		if strings.TrimSpace(sub.Missing) == "" || strings.TrimSpace(sub.ReplaceWith) == "" {
			continue
		}
		substitutions = append(substitutions, sub)
		names = append(names, strings.TrimSpace(sub.ReplaceWith))
	}

	resolved, unresolved, err := v.catalog.ResolveNames(ctx, names)
	if err != nil {
		return Result{}, err
	}

	byName := make(map[string]catalog.Ingredient, len(resolved))
	checked := make([]Ingredient, 0, len(resolved))
	for _, res := range resolved {
		byName[strings.ToLower(res.Input)] = res.Ingredient
		checked = append(checked, Ingredient{IngredientID: res.Ingredient.ID, CanonicalName: res.Ingredient.CanonicalName})
	}

	baseLines, err := v.linesStillNeeded(ctx, in.BaseLines[out.BaseRecipeID], substitutions)
	if err != nil {
		return Result{}, err
	}
	for _, line := range baseLines {
		checked = append(checked, Ingredient{IngredientID: line.IngredientID, CanonicalName: line.CanonicalName})
	}

	if len(in.DenyAllergens) > 0 {
		ids := make([]int64, 0, len(checked))
		for _, ing := range checked {
			ids = append(ids, ing.IngredientID)
		}
		allergens, err := v.catalog.AllergensForIngredients(ctx, ids)
		if err != nil {
			return Result{}, err
		}
		for _, ing := range checked {
			for _, tag := range allergens[ing.IngredientID] {
				if slices.Contains(in.DenyAllergens, tag) {
					return Result{}, &Rejection{
						Reason: RejectAllergen,
						Detail: fmt.Sprintf("%q carries denied allergen %q", ing.CanonicalName, tag),
					}
				}
			}
		}
		// Allergens are compared by canonical id only, so a name without one cannot
		// be cleared. Matching its text against allergen words would let a spelling
		// difference through.
		if len(unresolved) > 0 {
			return Result{}, &Rejection{
				Reason: RejectUnverifiable,
				Detail: fmt.Sprintf("cannot check allergens of names outside the catalog: %s", strings.Join(unresolved, ", ")),
			}
		}
	}

	have := make(map[int64]struct{}, len(in.HaveIDs))
	for _, id := range in.HaveIDs {
		have[id] = struct{}{}
	}

	result := Result{
		BaseRecipeID:  out.BaseRecipeID,
		Title:         fallbackString(out.Title, base.Title),
		Servings:      in.Servings,
		Adapted:       true,
		Used:          []Ingredient{},
		Missing:       []Ingredient{},
		Substitutions: []Substitution{},
		Steps:         nonEmpty(out.Steps),
		Base:          base,
	}

	// Amounts come from the corpus row for this recipe: the model returns bare
	// names, and an invented quantity would be worse than none.
	amounts := make(map[int64]catalog.RecipeIngredient, len(in.BaseLines[out.BaseRecipeID]))
	for _, ri := range in.BaseLines[out.BaseRecipeID] {
		amounts[ri.IngredientID] = ri
	}

	seen := map[int64]struct{}{}
	for _, item := range claimed {
		ing, ok := byName[strings.ToLower(strings.TrimSpace(item))]
		if !ok {
			continue
		}
		if _, dup := seen[ing.ID]; dup {
			continue
		}
		seen[ing.ID] = struct{}{}

		entry := Ingredient{IngredientID: ing.ID, CanonicalName: ing.CanonicalName}
		if line, ok := amounts[ing.ID]; ok {
			entry.Qty, entry.Unit = line.Qty, line.Unit
		}
		if _, inFridge := have[ing.ID]; inFridge || ing.IsStaple {
			result.Used = append(result.Used, entry)
		} else {
			result.Missing = append(result.Missing, entry)
		}
	}

	// The prompt asks the model to list everything its steps rely on, but a line it
	// forgot would otherwise surface only mid-cooking. A line the steps never mention
	// stays out: the model often drops one without recording a substitution, and
	// telling the user to buy fish for a chicken dish is plainly wrong. This match is
	// about the shopping list only; allergens are checked by id above, for every line.
	stepWords := make(map[string]struct{})
	for _, step := range result.Steps {
		for _, word := range splitWords(step) {
			stepWords[word] = struct{}{}
		}
	}
	for _, line := range baseLines {
		if _, dup := seen[line.IngredientID]; dup {
			continue
		}
		seen[line.IngredientID] = struct{}{}
		if _, inFridge := have[line.IngredientID]; inFridge {
			continue
		}
		if !mentions(stepWords, line.CanonicalName) {
			continue
		}
		result.Missing = append(result.Missing, Ingredient{
			IngredientID:  line.IngredientID,
			CanonicalName: line.CanonicalName,
			Qty:           line.Qty,
			Unit:          line.Unit,
		})
	}

	// Without a deny-list a name outside the catalog is harmless to keep, so it is
	// reported rather than silently kept. RecipeIngredient requires an id, which
	// such a name does not have, so it cannot be listed in missing[].
	if len(unresolved) > 0 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("could not match to the ingredient catalog: %s", strings.Join(unresolved, ", ")))
	}

	for _, sub := range substitutions {
		result.Substitutions = append(result.Substitutions, Substitution{
			Original:    sub.Missing,
			Replacement: sub.ReplaceWith,
			Reason:      sub.Note,
		})
	}
	return result, nil
}

// linesStillNeeded drops the base lines the model explicitly replaced: telling the
// user to buy what was just swapped out would be wrong. The name being replaced is
// only used to find the line, so it not resolving is not an error.
func (v *Validator) linesStillNeeded(ctx context.Context, lines []catalog.RecipeIngredient, subs []LLMSubstitution) ([]catalog.RecipeIngredient, error) {
	originals := make([]string, 0, len(subs))
	for _, sub := range subs {
		originals = append(originals, strings.TrimSpace(sub.Missing))
	}
	resolved, _, err := v.catalog.ResolveNames(ctx, originals)
	if err != nil {
		return nil, err
	}
	replaced := make(map[int64]struct{}, len(resolved))
	for _, res := range resolved {
		replaced[res.Ingredient.ID] = struct{}{}
	}

	needed := make([]catalog.RecipeIngredient, 0, len(lines))
	for _, line := range lines {
		if _, ok := replaced[line.IngredientID]; !ok {
			needed = append(needed, line)
		}
	}
	return needed, nil
}

// mentions reports whether every word of an ingredient name occurs in words, as a
// whole word and ignoring a plural ending. Requiring all of them keeps "chicken
// fillet" in the steps from counting as "salmon fillet".
func mentions(words map[string]struct{}, name string) bool {
	parts := splitWords(name)
	for _, part := range parts {
		found := false
		for _, form := range []string{part, part + "s", part + "es", strings.TrimSuffix(part, "s"), strings.TrimSuffix(part, "es")} {
			if _, ok := words[form]; ok {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(parts) > 0
}

func splitWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func fallbackString(primary, fallback string) string {
	if strings.TrimSpace(primary) == "" {
		return fallback
	}
	return primary
}
