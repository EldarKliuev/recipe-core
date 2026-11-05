package cook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/catalog"
	"github.com/recipe-ai/core-api/internal/inventory"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/recipe-ai/core-api/internal/recipehistory"
	"github.com/recipe-ai/core-api/internal/user"
	"github.com/redis/go-redis/v9"
)

// The corpus the fakes below serve. Recipe 100 is the one the model is expected
// to answer about; 101 exists so the candidate set is genuinely a set.
var (
	testRecipes = map[int64]catalog.Recipe{
		100: {ID: 100, Title: "Chicken and rice", NumSteps: 3, NumIngredients: 3},
		101: {ID: 101, Title: "Fried rice", NumSteps: 2, NumIngredients: 2},
	}
	testRecipeIngredients = map[int64][]catalog.RecipeIngredient{
		100: {
			{IngredientID: 1, CanonicalName: "chicken breast", RawText: "2 chicken breasts"},
			{IngredientID: 2, CanonicalName: "rice", RawText: "1 cup rice"},
			{IngredientID: 3, CanonicalName: "soy sauce", RawText: "2 tbsp soy sauce"},
		},
		101: {
			{IngredientID: 2, CanonicalName: "rice", RawText: "1 cup rice"},
			{IngredientID: 4, CanonicalName: "butter", RawText: "1 tbsp butter"},
		},
	}
)

type fakeRetrieval struct {
	rows        []coverageRow
	haveIDs     []int64
	retrievable bool
	err         error
}

func (f *fakeRetrieval) Coverage(context.Context, Query) ([]coverageRow, error) {
	return f.rows, f.err
}

func (f *fakeRetrieval) IsRetrievableFor(context.Context, Query, int64) (bool, error) {
	return f.retrievable, f.err
}

func (f *fakeRetrieval) HaveIngredientIDs(context.Context, uuid.UUID) ([]int64, error) {
	return f.haveIDs, f.err
}

type fakeCorpus struct{}

func (fakeCorpus) RecipesByIDs(_ context.Context, ids []int64) (map[int64]catalog.Recipe, error) {
	out := map[int64]catalog.Recipe{}
	for _, id := range ids {
		if rec, ok := testRecipes[id]; ok {
			out[id] = rec
		}
	}
	return out, nil
}

func (fakeCorpus) IngredientsForRecipes(_ context.Context, ids []int64) (map[int64][]catalog.RecipeIngredient, error) {
	out := map[int64][]catalog.RecipeIngredient{}
	for _, id := range ids {
		if ing, ok := testRecipeIngredients[id]; ok {
			out[id] = ing
		}
	}
	return out, nil
}

func (fakeCorpus) Directions(_ context.Context, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	for _, id := range ids {
		out[id] = []string{"Corpus step one.", "Corpus step two."}
	}
	return out, nil
}

func (fakeCorpus) IngredientsByIDs(_ context.Context, ids []int64) ([]catalog.Ingredient, error) {
	out := make([]catalog.Ingredient, 0, len(ids))
	for _, id := range ids {
		out = append(out, catalog.Ingredient{ID: id, CanonicalName: fmt.Sprintf("ingredient-%d", id)})
	}
	return out, nil
}

type fakePrefs struct{ prefs user.Prefs }

func (f fakePrefs) Prefs(context.Context, uuid.UUID) (user.Prefs, error) { return f.prefs, nil }

type fakeFridge struct{ items []inventory.Item }

func (f fakeFridge) List(context.Context, uuid.UUID) ([]inventory.Item, error) { return f.items, nil }

// scriptedLLM returns a queued reply (or error) per call, so a test can describe
// "rejected, then accepted" as data.
type scriptedLLM struct {
	replies []llmReply
	calls   int
}

type llmReply struct {
	resp LLMAdaptResponse
	err  error
}

func (s *scriptedLLM) Adapt(context.Context, LLMAdaptRequest) (LLMAdaptResponse, error) {
	i := s.calls
	s.calls++
	if i >= len(s.replies) {
		return LLMAdaptResponse{}, errors.New("scriptedLLM: unexpected extra call")
	}
	return s.replies[i].resp, s.replies[i].err
}

type recordingStore struct {
	adapted []recipehistory.Entry
	runs    []recipehistory.Run
}

func (r *recordingStore) SaveAdapted(_ context.Context, a recipehistory.Entry) error {
	r.adapted = append(r.adapted, a)
	return nil
}

func (r *recordingStore) SaveRun(_ context.Context, run recipehistory.Run) error {
	r.runs = append(r.runs, run)
	return nil
}

// harness wires a Service and Worker over miniredis plus in-memory fakes. It is
// the whole adaptation path minus Postgres and llm-service.
type harness struct {
	svc    *Service
	worker *Worker
	jobs   *JobStore
	llm    *scriptedLLM
	store  *recordingStore
	userID uuid.UUID
}

func newHarness(t *testing.T, replies []llmReply, maxTries int) *harness {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	jobs := NewJobStore(rdb, time.Minute)
	llm := &scriptedLLM{replies: replies}
	store := &recordingStore{}
	validator, _ := testValidator()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	worker := NewWorker(4, 1, llm, validator, jobs, store, time.Second, maxTries, log)
	retrieval := &fakeRetrieval{
		rows:        []coverageRow{{RecipeID: 100, TotalCount: 3, MissingCount: 1}, {RecipeID: 101, TotalCount: 2, MissingCount: 1}},
		haveIDs:     []int64{1, 2},
		retrievable: true,
	}
	fridge := fakeFridge{items: []inventory.Item{
		{IngredientID: 1, CanonicalName: "chicken breast", UseSoon: true},
		{IngredientID: 2, CanonicalName: "rice"},
	}}
	svc := NewService(retrieval, fakeCorpus{}, fakePrefs{}, fridge, jobs, worker)
	worker.Bind(svc)

	return &harness{svc: svc, worker: worker, jobs: jobs, llm: llm, store: store, userID: uuid.New()}
}

// runJob executes one task synchronously, bypassing the pool so assertions do
// not race with goroutine scheduling.
func (h *harness) runJob(t *testing.T, prefs user.Prefs) Job {
	t.Helper()
	ctx := context.Background()
	job := Job{ID: uuid.New(), UserID: h.userID, RecipeID: 100, Status: JobQueued, CreatedAt: time.Now().UTC()}
	if err := h.jobs.Save(ctx, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	h.worker.run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), task{
		Job:            job,
		IdempotencyKey: "key-1",
		CandidateIDs:   []int64{100, 101},
		Prefs:          prefs,
	})
	got, err := h.jobs.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("read job back: %v", err)
	}
	return got
}

func goodResponse() LLMAdaptResponse {
	return LLMAdaptResponse{
		Recipe: LLMAdaptedRecipe{
			BaseRecipeID:    100,
			Title:           "Chicken rice bowl",
			UsedIngredients: []string{"chicken breast", "rice"},
			Steps:           []string{"Cook the rice.", "Sear the chicken."},
		},
		Meta: RunMeta{Provider: "lmstudio", Model: "qwen2.5-7b-instruct", PromptVersion: "v1"},
	}
}

func TestWorkerStoresValidatedResultAndRecordsTheRun(t *testing.T) {
	h := newHarness(t, []llmReply{{resp: goodResponse()}}, 2)

	job := h.runJob(t, user.Prefs{})

	if job.Status != JobDone {
		t.Fatalf("Status = %q, want %q (error: %s)", job.Status, JobDone, job.Error)
	}
	if job.Result == nil {
		t.Fatal("Result = nil, want the validated adaptation")
	}
	if !job.Result.Adapted {
		t.Errorf("Adapted = false, want true; fallback reason %q", job.Result.FallbackReason)
	}
	if job.Result.Model != "qwen2.5-7b-instruct" {
		t.Errorf("Model = %q, want the model reported by llm-service", job.Result.Model)
	}
	if h.llm.calls != 1 {
		t.Errorf("llm calls = %d, want 1 for an answer accepted first time", h.llm.calls)
	}
	if len(h.store.adapted) != 1 {
		t.Errorf("saved adapted recipes = %d, want 1", len(h.store.adapted))
	}
	if len(h.store.runs) != 1 || h.store.runs[0].Status != recipehistory.RunSuccess {
		t.Errorf("runs = %+v, want a single successful run", h.store.runs)
	}
}

// A busy GPU is an availability problem, not a wrong answer: the job must fail
// with the "model busy" message and must not burn retries.
func TestWorkerFailsWithoutRetryWhenLLMServiceIsUnavailable(t *testing.T) {
	h := newHarness(t, []llmReply{
		{err: fmt.Errorf("%w: llm-service is busy", httpx.ErrUnavailable)},
	}, 3)

	job := h.runJob(t, user.Prefs{})

	if job.Status != JobFailed {
		t.Fatalf("Status = %q, want %q", job.Status, JobFailed)
	}
	if !strings.Contains(job.Error, "busy or unavailable") {
		t.Errorf("Error = %q, want it to explain the model is unavailable", job.Error)
	}
	if h.llm.calls != 1 {
		t.Errorf("llm calls = %d, want 1: an unavailable service must not be retried here", h.llm.calls)
	}
	// The key is released so the user can press the button again.
	if _, fresh, err := h.jobs.ClaimIdempotencyKey(context.Background(), h.userID, "key-1", uuid.New()); err != nil || !fresh {
		t.Errorf("ClaimIdempotencyKey fresh = %v (err %v), want the key released after a failure", fresh, err)
	}
}

// A 422 means llm-service cannot produce schema-valid output for this input, so
// retrying is pointless: serve the recipe unadapted instead.
func TestWorkerFallsBackImmediatelyOnUnusableModelOutput(t *testing.T) {
	h := newHarness(t, []llmReply{{err: fmt.Errorf("%w: schema repair exhausted", ErrModelUnusable)}}, 3)

	job := h.runJob(t, user.Prefs{})

	if job.Status != JobDone {
		t.Fatalf("Status = %q, want %q: a fallback is still a usable answer", job.Status, JobDone)
	}
	if job.Result == nil || job.Result.Adapted {
		t.Fatalf("Result = %+v, want an unadapted fallback", job.Result)
	}
	if job.Result.BaseRecipeID != 100 {
		t.Errorf("BaseRecipeID = %d, want the best candidate 100", job.Result.BaseRecipeID)
	}
	// Corpus directions are an internal reference and must never reach a client.
	if len(job.Result.Steps) != 0 {
		t.Errorf("Steps = %v, want none: verbatim corpus directions must not be served", job.Result.Steps)
	}
	if h.llm.calls != 1 {
		t.Errorf("llm calls = %d, want 1: a 422 must not be retried", h.llm.calls)
	}
}

// A 400 from llm-service means the two services disagree about the contract. It
// must fail loudly rather than being retried and then reported to the user as
// "the model output was rejected", which hides a bug on our side.
func TestWorkerFailsLoudlyWhenLLMServiceRejectsOurPayload(t *testing.T) {
	h := newHarness(t, []llmReply{
		{err: fmt.Errorf("%w: invalid_request: request payload is invalid", ErrRequestRejected)},
	}, 3)

	job := h.runJob(t, user.Prefs{})

	if job.Status != JobFailed {
		t.Fatalf("Status = %q, want %q for a contract mismatch", job.Status, JobFailed)
	}
	if job.Result != nil {
		t.Errorf("Result = %+v, want none: a fallback would hide the bug", job.Result)
	}
	if !strings.Contains(job.Error, "bug on our side") {
		t.Errorf("Error = %q, want it to say the fault is ours, not the model's", job.Error)
	}
	if h.llm.calls != 1 {
		t.Errorf("llm calls = %d, want 1: a rejected payload must not be retried", h.llm.calls)
	}
}

// Rejections are the case worth retrying, and the reason shown to the user must
// match the number of attempts actually made.
func TestWorkerRetriesRejectionsThenFallsBackWithAnAccurateReason(t *testing.T) {
	wrongRecipe := goodResponse()
	wrongRecipe.Recipe.BaseRecipeID = 999

	h := newHarness(t, []llmReply{{resp: wrongRecipe}, {resp: wrongRecipe}}, 2)

	job := h.runJob(t, user.Prefs{})

	if job.Status != JobDone {
		t.Fatalf("Status = %q, want %q", job.Status, JobDone)
	}
	if job.Result == nil || job.Result.Adapted {
		t.Fatalf("Result = %+v, want an unadapted fallback", job.Result)
	}
	if !strings.Contains(job.Result.FallbackReason, "rejected") ||
		!strings.Contains(job.Result.FallbackReason, "all 2 attempts") {
		t.Errorf("FallbackReason = %q, want it to report a rejection over the 2 attempts actually made", job.Result.FallbackReason)
	}
	if h.llm.calls != 2 {
		t.Errorf("llm calls = %d, want maxTries=2", h.llm.calls)
	}
	if len(h.store.runs) != 2 {
		t.Fatalf("runs = %d, want one recorded per rejected attempt", len(h.store.runs))
	}
	for _, run := range h.store.runs {
		if run.Status != recipehistory.RunRejected {
			t.Errorf("run status = %q, want %q", run.Status, recipehistory.RunRejected)
		}
	}
}

// fallback_reason reaches the client, so an upstream error body must stay in the
// logs and generation_runs instead.
func TestWorkerFallbackReasonDoesNotExposeUpstreamErrors(t *testing.T) {
	const upstreamDetail = "llm-service returned 500: traceback in /app/usecases/adapt.py"
	upstream := errors.New(upstreamDetail)

	h := newHarness(t, []llmReply{{err: upstream}, {err: upstream}}, 2)

	job := h.runJob(t, user.Prefs{})

	if job.Status != JobDone || job.Result == nil || job.Result.Adapted {
		t.Fatalf("job = %+v, want an unadapted fallback", job)
	}
	reason := job.Result.FallbackReason
	if strings.Contains(reason, "traceback") || strings.Contains(reason, "500") {
		t.Errorf("FallbackReason = %q, must not contain the upstream error", reason)
	}
	if !strings.Contains(reason, "all 2 attempts") {
		t.Errorf("FallbackReason = %q, want it to report the 2 attempts actually made", reason)
	}
	if len(h.store.runs) != 2 {
		t.Fatalf("runs = %d, want one recorded per failed attempt", len(h.store.runs))
	}
	for _, run := range h.store.runs {
		if run.Error != upstreamDetail {
			t.Errorf("run error = %q, want the upstream detail kept for diagnosis", run.Error)
		}
	}
}

// A first-attempt rejection followed by a good answer must end adapted: the
// retry exists precisely so one bad sample does not cost the user their result.
func TestWorkerAcceptsAnAnswerAfterAnEarlierRejection(t *testing.T) {
	wrongRecipe := goodResponse()
	wrongRecipe.Recipe.BaseRecipeID = 999

	h := newHarness(t, []llmReply{{resp: wrongRecipe}, {resp: goodResponse()}}, 2)

	job := h.runJob(t, user.Prefs{})

	if job.Status != JobDone || job.Result == nil || !job.Result.Adapted {
		t.Fatalf("job = %+v, want an adapted result on the second attempt", job)
	}
	if job.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", job.Attempts)
	}
}

// The allergen deny-list is the one rule that must never degrade to a warning.
func TestWorkerFallsBackRatherThanServingADeniedAllergen(t *testing.T) {
	withSoy := goodResponse()
	withSoy.Recipe.MissingIngredients = []string{"soy sauce"}

	h := newHarness(t, []llmReply{{resp: withSoy}}, 1)

	job := h.runJob(t, user.Prefs{Allergens: []string{"soy"}})

	if job.Result == nil || job.Result.Adapted {
		t.Fatalf("Result = %+v, want the allergen answer discarded", job.Result)
	}
	if len(h.store.runs) != 1 || h.store.runs[0].Status != recipehistory.RunRejected {
		t.Errorf("runs = %+v, want the attempt recorded as rejected", h.store.runs)
	}
}

// Generations are slow and serialised behind one GPU, so a double-submitted form
// must return the first job instead of queueing a second one.
func TestAdaptReplaysTheOriginalJobForARepeatedIdempotencyKey(t *testing.T) {
	h := newHarness(t, nil, 1)
	ctx := context.Background()

	first, err := h.svc.Adapt(ctx, h.userID, "form-submit-1", AdaptRequest{RecipeID: 100})
	if err != nil {
		t.Fatalf("first Adapt() error = %v", err)
	}
	second, err := h.svc.Adapt(ctx, h.userID, "form-submit-1", AdaptRequest{RecipeID: 100})
	if err != nil {
		t.Fatalf("replayed Adapt() error = %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("job ids = %s and %s, want the replay to return the original job", first.ID, second.ID)
	}
	if got := len(h.worker.queue); got != 1 {
		t.Errorf("queued tasks = %d, want 1: the replay must not queue a second generation", got)
	}
}

func TestAdaptRequiresAnIdempotencyKey(t *testing.T) {
	h := newHarness(t, nil, 1)

	_, err := h.svc.Adapt(context.Background(), h.userID, "  ", AdaptRequest{RecipeID: 100})
	if !errors.Is(err, httpx.ErrInvalidInput) {
		t.Errorf("Adapt() error = %v, want %v", err, httpx.ErrInvalidInput)
	}
}

// Asking to adapt a recipe the user was never offered is the user's mistake, so
// it is a 400 up front rather than a job that fails later in the validator.
func TestAdaptRejectsARecipeOutsideTheUsersMatches(t *testing.T) {
	h := newHarness(t, nil, 1)
	h.svc.retrieval.(*fakeRetrieval).retrievable = false

	_, err := h.svc.Adapt(context.Background(), h.userID, "key-1", AdaptRequest{RecipeID: 100})
	if !errors.Is(err, httpx.ErrInvalidInput) {
		t.Errorf("Adapt() error = %v, want %v", err, httpx.ErrInvalidInput)
	}
}

// Honest backpressure: a full queue is reported as unavailable instead of making
// the HTTP request wait on a single GPU.
func TestAdaptReportsUnavailableWhenTheQueueIsFull(t *testing.T) {
	h := newHarness(t, nil, 1)
	ctx := context.Background()

	for i := 0; i < cap(h.worker.queue); i++ {
		if _, err := h.svc.Adapt(ctx, h.userID, fmt.Sprintf("key-%d", i), AdaptRequest{RecipeID: 100}); err != nil {
			t.Fatalf("Adapt() %d error = %v", i, err)
		}
	}

	_, err := h.svc.Adapt(ctx, h.userID, "one-too-many", AdaptRequest{RecipeID: 100})
	if !errors.Is(err, httpx.ErrUnavailable) {
		t.Errorf("Adapt() error = %v, want %v once the queue is saturated", err, httpx.ErrUnavailable)
	}
}

// Job ids must not be probeable across accounts.
func TestJobIsNotReadableByAnotherUser(t *testing.T) {
	h := newHarness(t, nil, 1)
	ctx := context.Background()

	job, err := h.svc.Adapt(ctx, h.userID, "key-1", AdaptRequest{RecipeID: 100})
	if err != nil {
		t.Fatalf("Adapt() error = %v", err)
	}

	if _, err := h.svc.Job(ctx, uuid.New(), job.ID); !errors.Is(err, httpx.ErrNotFound) {
		t.Errorf("Job() error = %v, want %v for a different user", err, httpx.ErrNotFound)
	}
}
