package inventory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/authctx"
)

// memStore keeps one user's fridge in memory, enough to drive the handler.
type memStore struct{ items []Item }

func (m *memStore) List(context.Context, uuid.UUID) ([]Item, error) { return m.items, nil }

func (m *memStore) Upsert(_ context.Context, _ uuid.UUID, id int64, qty *float64, unit *string, exp *time.Time) error {
	m.items = append(m.items, Item{IngredientID: id, CanonicalName: "milk", Qty: qty, Unit: unit, ExpiresAt: exp})
	return nil
}

func (m *memStore) Delete(context.Context, uuid.UUID, int64) error { return nil }

func serve(t *testing.T, h *Handler, method, body string) []byte {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req = req.WithContext(authctx.With(req.Context(), uuid.New()))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("%s: status %d, body %s", method, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func TestAddReturnsExpiryAsTheSameDateItAccepted(t *testing.T) {
	h := NewHandler(&memStore{}, nil)

	out := serve(t, h, http.MethodPost, `{"ingredient_id": 7, "expires_at": "2026-11-08"}`)

	var resp struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Item["expires_at"]; got != "2026-11-08" {
		t.Fatalf("expires_at = %v, want 2026-11-08", got)
	}
}

func TestListWritesExpiryAsDateOnly(t *testing.T) {
	// A date read back in a non-UTC location must keep its own calendar day,
	// not be shifted to UTC.
	late := time.Date(2026, 11, 8, 23, 30, 0, 0, time.FixedZone("UTC+5", 5*3600))
	store := &memStore{items: []Item{
		{IngredientID: 1, CanonicalName: "milk", ExpiresAt: &late, UseSoon: true},
		{IngredientID: 2, CanonicalName: "rice"},
	}}
	h := NewHandler(store, nil)

	out := serve(t, h, http.MethodGet, "")

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(resp.Items))
	}
	if got := resp.Items[0]["expires_at"]; got != "2026-11-08" {
		t.Fatalf("expires_at = %v, want 2026-11-08", got)
	}
	if got := resp.Items[0]["use_soon"]; got != true {
		t.Fatalf("use_soon = %v, want true: the other fields must still be written", got)
	}
	if _, ok := resp.Items[1]["expires_at"]; ok {
		t.Fatalf("item without expiry has expires_at: %v", resp.Items[1])
	}
}
