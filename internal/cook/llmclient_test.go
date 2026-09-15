package cook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

// Contract tests against a stub llm-service. They pin the parts of the boundary
// core-api actually depends on: the request shape, and how each failure status is
// classified, since that classification decides whether a job retries, falls back
// or fails.

// The response body below is llm-service's AdaptResponse shape verbatim, so this
// also pins that we read `meta` (not `run`) and `completion_tokens` (not
// `output_tokens`).
func TestAdaptSendsRecipesAndDecodesTheResponse(t *testing.T) {
	var gotPath, gotContentType string
	var gotBody LLMAdaptRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := decodeJSON(r, &gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"recipe": {
				"base_recipe_id": 42,
				"title": "Fridge fried rice",
				"used_ingredients": ["rice"],
				"missing_ingredients": [],
				"substitutions": [{"missing": "mirin", "replace_with": "sugar", "note": "not in fridge"}],
				"steps": ["Fry the rice."]
			},
			"meta": {"provider": "lmstudio", "model": "qwen2.5-7b", "prompt_version": "v3",
			         "latency_ms": 8123, "queue_wait_ms": 12,
			         "prompt_tokens": 900, "completion_tokens": 300, "retries": 1}
		}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, 5*time.Second)
	resp, err := client.Adapt(context.Background(), LLMAdaptRequest{
		Recipes:   []LLMRetrievedRecipe{{ID: 42, Title: "Fried rice"}},
		Inventory: []string{"rice (use soon)"},
		Prefs:     LLMPrefs{Allergies: []string{"peanuts"}},
	})
	if err != nil {
		t.Fatalf("Adapt() error = %v", err)
	}

	if gotPath != "/v1/adapt" {
		t.Errorf("path = %q, want /v1/adapt", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if len(gotBody.Recipes) != 1 || gotBody.Recipes[0].ID != 42 {
		t.Errorf("recipes = %+v, want the single recipe we passed", gotBody.Recipes)
	}
	if len(gotBody.Inventory) != 1 || gotBody.Inventory[0] != "rice (use soon)" {
		t.Errorf("inventory = %v, want the fridge line as a plain string", gotBody.Inventory)
	}
	if len(gotBody.Prefs.Allergies) != 1 || gotBody.Prefs.Allergies[0] != "peanuts" {
		t.Errorf("prefs.allergies = %v, want [peanuts]", gotBody.Prefs.Allergies)
	}

	if resp.Recipe.BaseRecipeID != 42 {
		t.Errorf("base_recipe_id = %d, want 42", resp.Recipe.BaseRecipeID)
	}
	if got := resp.Recipe.UsedIngredients; len(got) != 1 || got[0] != "rice" {
		t.Errorf("used_ingredients = %v, want the bare names the model returns", got)
	}
	if resp.Meta.Model != "qwen2.5-7b" || resp.Meta.PromptVersion != "v3" || resp.Meta.Retries != 1 {
		t.Errorf("meta = %+v, want it decoded for generation_runs", resp.Meta)
	}
	if resp.Meta.CompletionTokens == nil || *resp.Meta.CompletionTokens != 300 {
		t.Errorf("meta.completion_tokens = %v, want 300", resp.Meta.CompletionTokens)
	}
	if len(resp.Recipe.Substitutions) != 1 || resp.Recipe.Substitutions[0].ReplaceWith != "sugar" {
		t.Errorf("substitutions = %+v, want one decoded swap", resp.Recipe.Substitutions)
	}
}

// Token counts are optional upstream, and a provider that omits them must not
// break decoding.
func TestAdaptToleratesMissingTokenCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"recipe":{"base_recipe_id":1,"title":"t","steps":["s"],
			"used_ingredients":[],"missing_ingredients":[],"substitutions":[]},
			"meta":{"provider":"lmstudio","model":"m","prompt_version":"v1","latency_ms":1,
			        "queue_wait_ms":0,"retries":0,"prompt_tokens":null,"completion_tokens":null}}`))
	}))
	defer srv.Close()

	resp, err := NewLLMClient(srv.URL, 5*time.Second).Adapt(context.Background(), LLMAdaptRequest{})
	if err != nil {
		t.Fatalf("Adapt() error = %v", err)
	}
	if resp.Meta.PromptTokens != nil || resp.Meta.CompletionTokens != nil {
		t.Errorf("token counts = %v/%v, want both nil", resp.Meta.PromptTokens, resp.Meta.CompletionTokens)
	}
}

// 503 means the single GPU is saturated. It must classify as unavailable so the
// worker fails the job with a "busy" message instead of retrying into the queue.
func TestAdaptClassifiesBusyAsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"model busy"}`))
	}))
	defer srv.Close()

	_, err := NewLLMClient(srv.URL, 5*time.Second).Adapt(context.Background(), LLMAdaptRequest{})
	if !errors.Is(err, httpx.ErrUnavailable) {
		t.Fatalf("error = %v, want it to wrap ErrUnavailable", err)
	}
}

// 422 means llm-service could not satisfy its own schema. Retrying is pointless,
// so it maps to a distinct sentinel that sends the job to the unadapted fallback.
func TestAdaptClassifiesUnprocessableAsModelUnusable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"schema repair failed after 3 attempts"}`))
	}))
	defer srv.Close()

	_, err := NewLLMClient(srv.URL, 5*time.Second).Adapt(context.Background(), LLMAdaptRequest{})
	if !errors.Is(err, ErrModelUnusable) {
		t.Fatalf("error = %v, want it to wrap ErrModelUnusable", err)
	}
	if errors.Is(err, httpx.ErrUnavailable) {
		t.Error("422 must not classify as unavailable; that would fail the job instead of falling back")
	}
}

// 504 is llm-service's provider_timeout. Like 503 it is an availability problem,
// so it must not be retried as though the model had answered badly.
func TestAdaptClassifiesProviderTimeoutAsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"error":{"code":"provider_timeout","message":"LM Studio timed out"}}`))
	}))
	defer srv.Close()

	_, err := NewLLMClient(srv.URL, 5*time.Second).Adapt(context.Background(), LLMAdaptRequest{})
	if !errors.Is(err, httpx.ErrUnavailable) {
		t.Fatalf("error = %v, want it to wrap ErrUnavailable", err)
	}
}

// 400 means the two services disagree about the contract. That is our bug: it
// must be distinguishable so the worker fails loudly instead of retrying and then
// blaming the model in a fallback message.
func TestAdaptClassifiesBadRequestAsOurBug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"request payload is invalid",
			"detail":[{"type":"missing","loc":["body","recipes"],"msg":"Field required"}]},
			"request_id":"abc"}`))
	}))
	defer srv.Close()

	_, err := NewLLMClient(srv.URL, 5*time.Second).Adapt(context.Background(), LLMAdaptRequest{})
	if !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("error = %v, want it to wrap ErrRequestRejected", err)
	}
	if errors.Is(err, ErrModelUnusable) || errors.Is(err, httpx.ErrUnavailable) {
		t.Error("a 400 must not be classified as a model or availability problem")
	}
	// The pydantic detail list is far too long for a user-facing message.
	if got := err.Error(); len(got) > 200 || !strings.Contains(got, "invalid_request") {
		t.Errorf("error = %q, want a short message naming the upstream code", got)
	}
}

func TestAdaptReportsUnreachableServiceAsUnavailable(t *testing.T) {
	// A port nothing listens on stands in for llm-service being down.
	_, err := NewLLMClient("http://127.0.0.1:1", time.Second).Adapt(context.Background(), LLMAdaptRequest{})
	if !errors.Is(err, httpx.ErrUnavailable) {
		t.Fatalf("error = %v, want it to wrap ErrUnavailable", err)
	}
}

func TestAdaptSurfacesUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	_, err := NewLLMClient(srv.URL, 5*time.Second).Adapt(context.Background(), LLMAdaptRequest{})
	if err == nil {
		t.Fatal("Adapt() = nil error, want a failure for 500")
	}
	if errors.Is(err, httpx.ErrUnavailable) || errors.Is(err, ErrModelUnusable) {
		t.Errorf("error = %v, want an unclassified failure so the job retries", err)
	}
}
