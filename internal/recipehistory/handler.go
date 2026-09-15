package recipehistory

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/authctx"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type store interface {
	List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]ListItem, error)
	SetRating(ctx context.Context, userID, adaptedID uuid.UUID, rating int16) error
}

type Handler struct {
	store store
}

func NewHandler(s store) *Handler {
	return &Handler{store: s}
}

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/{adaptedID}/rating", h.rate)
	return r
}

type listResponse struct {
	Items []ListItem `json:"items"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	limit, err := httpx.QueryInt(r, "limit", 20, 1, 100)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	offset, err := httpx.QueryInt(r, "offset", 0, 0, 10000)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items, err := h.store.List(r.Context(), userID, limit, offset)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, listResponse{Items: items})
}

type ratingRequest struct {
	Rating int16 `json:"rating"`
}

func (h *Handler) rate(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	adaptedID, err := uuid.Parse(chi.URLParam(r, "adaptedID"))
	if err != nil {
		httpx.Fail(w, r, httpx.Invalidf("adaptedID must be a uuid"))
		return
	}
	var req ratingRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.Rating < 1 || req.Rating > 5 {
		httpx.Fail(w, r, httpx.Invalidf("rating must be between 1 and 5"))
		return
	}
	if err := h.store.SetRating(r.Context(), userID, adaptedID, req.Rating); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w)
}
