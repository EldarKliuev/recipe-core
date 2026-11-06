package cook

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

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
//   - any ingredient or substitution replacement carrying a denied allergen tag
//     rejects the whole answer;
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
	ids := make([]int64, 0, len(resolved))
	for _, res := range resolved {
		byName[strings.ToLower(res.Input)] = res.Ingredient
		ids = append(ids, res.Ingredient.ID)
	}

	if len(in.DenyAllergens) > 0 {
		allergens, err := v.catalog.AllergensForIngredients(ctx, ids)
		if err != nil {
			return Result{}, err
		}
		for _, res := range resolved {
			for _, tag := range allergens[res.Ingredient.ID] {
				if slices.Contains(in.DenyAllergens, tag) {
					return Result{}, &Rejection{
						Reason: RejectAllergen,
						Detail: fmt.Sprintf("%q carries denied allergen %q", res.Ingredient.CanonicalName, tag),
					}
				}
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

	// Ingredients the model invented that do not exist in the catalog cannot be
	// verified, so they are reported rather than silently kept.
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
