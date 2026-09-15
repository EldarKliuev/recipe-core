//go:build integration

// End-to-end test of the adaptation path over real Postgres repositories and a
// stub llm-service. The stub validates the request the way llm-service's pydantic
// models do, so a drift in the outbound contract fails here rather than in
// production: a field name we get wrong upstream is silently dropped (its schemas
// declare extra="ignore") and then reported as missing.
//
//	make test-integration
package cook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/catalog"
	"github.com/recipe-ai/core-api/internal/inventory"
	"github.com/recipe-ai/core-api/internal/recipehistory"
	"github.com/recipe-ai/core-api/internal/user"
	"github.com/redis/go-redis/v9"
)

// stubRequest is the payload as llm-service declares it, with everything required
// so that a missing or misnamed field fails to decode.
type stubRequest struct {
	Recipes *[]struct {
		ID          *int64    `json:"id"`
		Title       *string   `json:"title"`
		Ingredients *[]string `json:"ingredients"`
		Steps       *[]string `json:"steps"`
	} `json:"recipes"`
	Inventory *[]string `json:"inventory"`
	Prefs     *struct {
		Allergies *[]string `json:"allergies"`
		Diet      *string   `json:"diet"`
		Dislikes  *[]string `json:"dislikes"`
		Servings  *int      `json:"servings"`
	} `json:"prefs"`
}

func (r stubRequest) validate() []string {
	var problems []string
	if r.Recipes == nil {
		problems = append(problems, "recipes: field required")
	} else if n := len(*r.Recipes); n < 1 || n > 5 {
		problems = append(problems, "recipes: must contain 1 to 5 items")
	} else {
		for i, rec := range *r.Recipes {
			if rec.ID == nil {
				problems = append(problems, "recipes.id: field required")
			}
			if rec.Title == nil {
				problems = append(problems, "recipes.title: field required")
			}
			if rec.Ingredients == nil {
				problems = append(problems, "recipes.ingredients: field required")
			}
			if rec.Steps == nil {
				problems = append(problems, "recipes.steps: field required")
			}
			_ = i
		}
	}
	if r.Inventory == nil {
		problems = append(problems, "inventory: field required")
	} else if len(*r.Inventory) < 1 {
		problems = append(problems, "inventory: must contain at least 1 item")
	}
	if r.Prefs == nil {
		problems = append(problems, "prefs: field required")
	} else {
		if r.Prefs.Allergies == nil {
			problems = append(problems, "prefs.allergies: field required")
		}
		if r.Prefs.Dislikes == nil {
			problems = append(problems, "prefs.dislikes: field required")
		}
	}
	return problems
}

// stubLLMService mimics llm-service. reply builds the adapted recipe from the
// request so the answer is always about a recipe that was actually offered.
func stubLLMService(t *testing.T, reply func(stubRequest) LLMAdaptedRecipe) (*httptest.Server, *stubRequest) {
	t.Helper()
	var seen stubRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/adapt" {
			http.Error(w, `{"error":{"code":"not_found","message":"no such route"}}`, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		// DisallowUnknownFields is deliberately not set: llm-service ignores
		// extras, and this test is about the fields it requires.
		if err := json.Unmarshal(body, &seen); err != nil {
			t.Errorf("stub could not decode the request: %v", err)
		}
		if problems := seen.validate(); len(problems) > 0 {
			t.Errorf("core-api sent a payload llm-service would reject: %s\nbody: %s",
				strings.Join(problems, "; "), body)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"request payload is invalid"}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(LLMAdaptResponse{
			Recipe: reply(seen),
			Meta: RunMeta{
				Provider: "stub", Model: "qwen/qwen3.5-9b", PromptVersion: "v1",
				LatencyMS: 1234, QueueWaitMS: 5,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// liveHarness wires the real repositories, the real HTTP client and a stub
// llm-service: everything except the model itself.
type liveHarness struct {
	svc     *Service
	worker  *Worker
	jobs    *JobStore
	history *recipehistory.Repo
	seed    corpusSeed
}

func newLiveHarness(t *testing.T, pool *pgxpool.Pool, baseURL string) *liveHarness {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	seed := seedCorpus(t, pool)
	jobs := NewJobStore(rdb, time.Minute)
	history := recipehistory.NewRepo(pool)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	worker := NewWorker(4, 1, NewLLMClient(baseURL, 10*time.Second),
		NewValidator(catalog.NewRepo(pool)), jobs, history, 10*time.Second, 2, log)
	svc := NewService(NewRetrievalRepo(pool), catalog.NewRepo(pool),
		user.NewRepo(pool), inventory.NewRepo(pool), jobs, worker)
	worker.Bind(svc)

	return &liveHarness{svc: svc, worker: worker, jobs: jobs, history: history, seed: seed}
}

// runOne drains exactly one queued task, so the assertions do not race the pool.
func (h *liveHarness) runOne(t *testing.T, job Job) Job {
	t.Helper()
	ctx := context.Background()

	select {
	case task := <-h.worker.queue:
		h.worker.run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), task)
	case <-time.After(time.Second):
		t.Fatal("no task was queued")
	}

	got, err := h.jobs.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	return got
}

func TestAdaptEndToEndProducesAValidatedResult(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()

	srv, seen := stubLLMService(t, func(req stubRequest) LLMAdaptedRecipe {
		chosen := (*req.Recipes)[0]
		return LLMAdaptedRecipe{
			BaseRecipeID:    *chosen.ID,
			Title:           "Spicy chicken and rice",
			Steps:           []string{"Rinse the rice.", "Sear the chicken.", "Simmer together."},
			UsedIngredients: []string{"chicken breast", "rice", "salt"},
			Substitutions: []LLMSubstitution{
				{Missing: "soy sauce", ReplaceWith: "salt", Note: "not in your fridge"},
			},
		}
	})
	h := newLiveHarness(t, pool, srv.URL)

	servings := 4
	job, err := h.svc.Adapt(context.Background(), h.seed.userID, "e2e-key-1", AdaptRequest{
		RecipeID: h.seed.chickenRice,
		Servings: &servings,
		Notes:    "make it spicier",
	})
	if err != nil {
		t.Fatalf("Adapt() error = %v", err)
	}
	if job.Status != JobQueued {
		t.Errorf("Status = %q, want %q immediately after POST", job.Status, JobQueued)
	}

	done := h.runOne(t, job)
	if done.Status != JobDone {
		t.Fatalf("Status = %q, want %q (error: %s)", done.Status, JobDone, done.Error)
	}
	if done.Result == nil || !done.Result.Adapted {
		t.Fatalf("Result = %+v, want an adapted answer", done.Result)
	}

	// What we sent: the recipe the user picked, the fridge as plain strings, and
	// the notes folded into the one free-text field llm-service reads.
	if got := *seen.Recipes; len(got) != 1 || *got[0].ID != h.seed.chickenRice {
		t.Errorf("recipes sent = %d entries, want just the requested recipe", len(got))
	}
	if got := *(*seen.Recipes)[0].Steps; len(got) == 0 {
		t.Error("no corpus directions were sent; the model has nothing to paraphrase")
	}
	if got := *seen.Inventory; len(got) == 0 {
		t.Error("inventory was empty, which llm-service rejects")
	}
	if seen.Prefs.Servings == nil || *seen.Prefs.Servings != 4 {
		t.Errorf("prefs.servings = %v, want 4", seen.Prefs.Servings)
	}
	if seen.Prefs.Diet == nil || !strings.Contains(*seen.Prefs.Diet, "make it spicier") {
		t.Errorf("prefs.diet = %v, want the user notes carried in it", seen.Prefs.Diet)
	}

	// What we returned: names resolved against the catalog, amounts from the
	// corpus, and the substitution mapped onto our own field names.
	res := done.Result
	if res.BaseRecipeID != h.seed.chickenRice {
		t.Errorf("base_recipe_id = %d, want %d", res.BaseRecipeID, h.seed.chickenRice)
	}
	if res.Servings == nil || *res.Servings != 4 {
		t.Errorf("servings = %v, want the requested 4 echoed back", res.Servings)
	}
	if len(res.Missing) != 0 {
		t.Errorf("missing = %v, want none: the fridge covers this recipe", names(res.Missing))
	}
	if got := names(res.Used); !equal(got, []string{"chicken breast", "rice", "salt"}) {
		t.Errorf("used = %v, want the three catalog-resolved ingredients", got)
	}
	for _, ing := range res.Used {
		if ing.IngredientID == 0 {
			t.Errorf("ingredient %q has no catalog id, so it was not resolved", ing.CanonicalName)
		}
	}
	if len(res.Substitutions) != 1 || res.Substitutions[0].Replacement != "salt" {
		t.Errorf("substitutions = %+v, want the swap mapped to original/replacement", res.Substitutions)
	}
	if len(res.Steps) != 3 {
		t.Errorf("steps = %v, want the three paraphrased steps", res.Steps)
	}
	if res.Model != "qwen/qwen3.5-9b" {
		t.Errorf("model = %q, want it recorded from the run metadata", res.Model)
	}

	// The adaptation and its telemetry must be persisted for /me/history.
	items, err := h.history.List(context.Background(), h.seed.userID, 10, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("history = %d entries, want 1", len(items))
	}
	if items[0].BaseRecipeID != h.seed.chickenRice || !items[0].Adapted {
		t.Errorf("history entry = %+v, want the adapted recipe recorded", items[0])
	}
}

// An answer naming an ingredient that carries a denied allergen must never be
// served, even when the model insists. The whole point of validating in core-api
// is that this does not depend on the prompt.
func TestAdaptEndToEndRejectsAnAllergenAnswerAndFallsBack(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()

	srv, _ := stubLLMService(t, func(req stubRequest) LLMAdaptedRecipe {
		return LLMAdaptedRecipe{
			BaseRecipeID:    *(*req.Recipes)[0].ID,
			Title:           "Creamy chicken and rice",
			Steps:           []string{"Stir in the heavy cream."},
			UsedIngredients: []string{"chicken breast", "rice", "heavy cream"},
		}
	})
	h := newLiveHarness(t, pool, srv.URL)

	ctx := context.Background()
	prefs := user.Prefs{Allergens: []string{"dairy"}}
	if err := user.NewRepo(pool).SavePrefs(ctx, h.seed.userID, prefs); err != nil {
		t.Fatalf("SavePrefs() error = %v", err)
	}

	job, err := h.svc.Adapt(ctx, h.seed.userID, "e2e-key-2", AdaptRequest{RecipeID: h.seed.chickenRice})
	if err != nil {
		t.Fatalf("Adapt() error = %v", err)
	}

	done := h.runOne(t, job)
	if done.Status != JobDone {
		t.Fatalf("Status = %q, want %q (error: %s)", done.Status, JobDone, done.Error)
	}
	if done.Result == nil || done.Result.Adapted {
		t.Fatalf("Result = %+v, want the dairy answer discarded in favour of a fallback", done.Result)
	}
	for _, ing := range append(done.Result.Used, done.Result.Missing...) {
		if ing.CanonicalName == "heavy cream" {
			t.Error("the fallback served a dairy ingredient to a dairy-allergic user")
		}
	}
	// Corpus directions are an internal reference and must not be published.
	if len(done.Result.Steps) != 0 {
		t.Errorf("steps = %v, want none in a fallback", done.Result.Steps)
	}

	// Both rejected attempts must be recorded so the rejection rate is measurable.
	var rejected int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM generation_runs WHERE user_id = $1 AND status = 'rejected'`,
		h.seed.userID).Scan(&rejected); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if rejected != 2 {
		t.Errorf("rejected runs = %d, want one per attempt", rejected)
	}
}
