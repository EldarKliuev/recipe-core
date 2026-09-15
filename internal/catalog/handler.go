package catalog

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type searcher interface {
	Search(ctx context.Context, query string, limit int) ([]Ingredient, error)
}

type Handler struct {
	repo searcher
}

func NewHandler(repo searcher) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.search)
	return r
}

type searchResponse struct {
	Items []Ingredient `json:"items"`
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) < 2 {
		httpx.Fail(w, r, httpx.Invalidf("q must be at least 2 characters"))
		return
	}
	limit, err := httpx.QueryInt(r, "limit", 20, 1, 50)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items, err := h.repo.Search(r.Context(), query, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if items == nil {
		items = []Ingredient{}
	}
	httpx.JSON(w, http.StatusOK, searchResponse{Items: items})
}
