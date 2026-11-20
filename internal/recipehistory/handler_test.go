package recipehistory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/authctx"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

// memStore holds entries per owner and answers Get the way Repo does: an
// entry of another user is not found.
type memStore struct {
	owner uuid.UUID
	items map[uuid.UUID]ListItem
}

func (m *memStore) List(context.Context, uuid.UUID, int, int) ([]ListItem, error) { return nil, nil }

func (m *memStore) Get(_ context.Context, userID, adaptedID uuid.UUID) (ListItem, error) {
	it, ok := m.items[adaptedID]
	if !ok || userID != m.owner {
		return ListItem{}, fmt.Errorf("%w: adapted recipe %s", httpx.ErrNotFound, adaptedID)
	}
	return it, nil
}

func (m *memStore) SetRating(context.Context, uuid.UUID, uuid.UUID, int16) error { return nil }

func get(h *Handler, userID uuid.UUID, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(authctx.With(req.Context(), userID))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func storeWithOneEntry() (*memStore, ListItem) {
	item := ListItem{
		Entry:     Entry{ID: uuid.New(), BaseRecipeID: 42, Result: []byte(`{"title":"Pancakes"}`), Adapted: true},
		BaseTitle: "Pancakes",
	}
	return &memStore{owner: uuid.New(), items: map[uuid.UUID]ListItem{item.ID: item}}, item
}

func TestGetReturnsTheOwnersEntry(t *testing.T) {
	store, item := storeWithOneEntry()

	rec := get(NewHandler(store), store.owner, "/"+item.ID.String())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != item.ID.String() || got["base_title"] != "Pancakes" || got["base_recipe_id"] != float64(42) {
		t.Errorf("body = %v, want the entry itself, not wrapped", got)
	}
	if _, ok := got["result"].(map[string]any); !ok {
		t.Errorf("result = %v, want the stored JSON object", got["result"])
	}
}

func TestGetAnswersNotFoundForAnotherUsersEntry(t *testing.T) {
	store, item := storeWithOneEntry()

	rec := get(NewHandler(store), uuid.New(), "/"+item.ID.String())

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGetRejectsANonUUIDID(t *testing.T) {
	store, _ := storeWithOneEntry()

	rec := get(NewHandler(store), store.owner, "/not-a-uuid")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
