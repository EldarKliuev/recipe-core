package user

import (
	"context"

	"net/http"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/authctx"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type prefsStore interface {
	Prefs(ctx context.Context, userID uuid.UUID) (Prefs, error)
	SavePrefs(ctx context.Context, userID uuid.UUID, p Prefs) error
}

// ingredientChecker lets prefs validation confirm that disliked ingredient ids
// actually exist in the catalog.
type ingredientChecker interface {
	ExistingIngredientIDs(ctx context.Context, ids []int64) ([]int64, error)
}

type Handler struct {
	store   prefsStore
	catalog ingredientChecker
}

func NewHandler(store prefsStore, catalog ingredientChecker) *Handler {
	return &Handler{store: store, catalog: catalog}
}

func (h *Handler) GetPrefs(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	prefs, err := h.store.Prefs(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, prefs)
}

func (h *Handler) PutPrefs(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var prefs Prefs
	if err := httpx.Decode(r, &prefs); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := prefs.Validate(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if len(prefs.DislikedIngredientIDs) > 0 {
		existing, err := h.catalog.ExistingIngredientIDs(r.Context(), prefs.DislikedIngredientIDs)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if len(existing) != len(prefs.DislikedIngredientIDs) {
			httpx.Fail(w, r, httpx.Invalidf("disliked_ingredient_ids contains unknown ingredient ids"))
			return
		}
	}
	if err := h.store.SavePrefs(r.Context(), userID, prefs); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, prefs)
}
