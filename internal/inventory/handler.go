package inventory

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/catalog"
	"github.com/recipe-ai/core-api/internal/platform/authctx"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type store interface {
	List(ctx context.Context, userID uuid.UUID) ([]Item, error)
	Upsert(ctx context.Context, userID uuid.UUID, ingredientID int64, qty *float64, unit *string, expiresAt *time.Time) error
	Delete(ctx context.Context, userID uuid.UUID, ingredientID int64) error
}

// resolver turns typed-in ingredient names into canonical catalog ids.
type resolver interface {
	ResolveNames(ctx context.Context, names []string) ([]catalog.Resolution, []string, error)
}

type Handler struct {
	store   store
	catalog resolver
}

func NewHandler(s store, c resolver) *Handler {
	return &Handler{store: s, catalog: c}
}

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/", h.add)
	r.Delete("/{ingredientID}", h.remove)
	return r
}

type listResponse struct {
	Items []Item `json:"items"`
}

// addRequest accepts either a resolved ingredient_id or a free-text name. The
// web client sends ids picked from autocomplete; name is the escape hatch for
// bulk entry and for the mobile client later.
type addRequest struct {
	IngredientID int64    `json:"ingredient_id,omitempty"`
	Name         string   `json:"name,omitempty"`
	Qty          *float64 `json:"qty,omitempty"`
	Unit         *string  `json:"unit,omitempty"`
	ExpiresAt    *string  `json:"expires_at,omitempty"`
}

type addResponse struct {
	Item Item `json:"item"`
	// Resolved reports the canonical name a free-text input was matched to, and
	// whether that match was exact, so the UI can ask for confirmation.
	ResolvedFrom string `json:"resolved_from,omitempty"`
	ExactMatch   bool   `json:"exact_match"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items, err := h.store.List(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, listResponse{Items: items})
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req addRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if (req.IngredientID == 0) == (req.Name == "") {
		httpx.Fail(w, r, httpx.Invalidf("provide exactly one of ingredient_id or name"))
		return
	}
	if req.Qty != nil && *req.Qty <= 0 {
		httpx.Fail(w, r, httpx.Invalidf("qty must be positive"))
		return
	}

	expiresAt, err := parseExpiry(req.ExpiresAt)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	resp := addResponse{ExactMatch: true}
	ingredientID := req.IngredientID
	var canonicalName string
	var category *string

	if req.Name != "" {
		resolved, unresolved, err := h.catalog.ResolveNames(r.Context(), []string{req.Name})
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if len(unresolved) > 0 || len(resolved) == 0 {
			httpx.Fail(w, r, httpx.Invalidf("%q does not match any known ingredient", req.Name))
			return
		}
		match := resolved[0]
		ingredientID = match.Ingredient.ID
		canonicalName = match.Ingredient.CanonicalName
		category = match.Ingredient.Category
		resp.ResolvedFrom = req.Name
		resp.ExactMatch = match.Exact
	}

	if err := h.store.Upsert(r.Context(), userID, ingredientID, req.Qty, req.Unit, expiresAt); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	item := Item{
		IngredientID:  ingredientID,
		CanonicalName: canonicalName,
		Category:      category,
		Qty:           req.Qty,
		Unit:          req.Unit,
		ExpiresAt:     expiresAt,
		UseSoon:       expiresAt != nil && expiresAt.Before(time.Now().Add(UseSoonWindow)),
	}
	if canonicalName == "" {
		// The id path skips resolution, so read back the name for the response.
		if items, err := h.store.List(r.Context(), userID); err == nil {
			for _, existing := range items {
				if existing.IngredientID == ingredientID {
					item = existing
					break
				}
			}
		}
	}
	resp.Item = item
	httpx.JSON(w, http.StatusCreated, resp)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ingredientID, err := strconv.ParseInt(chi.URLParam(r, "ingredientID"), 10, 64)
	if err != nil {
		httpx.Fail(w, r, httpx.Invalidf("ingredientID must be an integer"))
		return
	}
	if err := h.store.Delete(r.Context(), userID, ingredientID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func parseExpiry(raw *string) (*time.Time, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.DateOnly, *raw)
	if err != nil {
		return nil, httpx.Invalidf("expires_at must be a YYYY-MM-DD date")
	}
	return &t, nil
}
