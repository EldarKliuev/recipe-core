package cook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/recipe-ai/core-api/internal/recipehistory"
	"github.com/recipe-ai/core-api/internal/user"
)

// task is one unit of adaptation work. It carries a snapshot of prefs and the
// candidate set so the worker never re-reads state the request already resolved.
type task struct {
	Job            Job
	IdempotencyKey string
	CandidateIDs   []int64
	Prefs          user.Prefs
	Servings       *int
	Notes          string
}

type adapter interface {
	Adapt(ctx context.Context, req LLMAdaptRequest) (LLMAdaptResponse, error)
}

type resultStore interface {
	SaveAdapted(ctx context.Context, a recipehistory.Entry) error
	SaveRun(ctx context.Context, run recipehistory.Run) error
}

// Worker is the in-process adaptation pool. It lives in the same binary on
// purpose: a separate queue service would add operational weight for a workload
// bounded by one local GPU.
type Worker struct {
	queue    chan task
	llm      adapter
	valid    *Validator
	jobs     *JobStore
	results  resultStore
	log      *slog.Logger
	svc      *Service
	timeout  time.Duration
	maxTries int

	wg   sync.WaitGroup
	size int
}

func NewWorker(
	queueSize, poolSize int,
	llmClient adapter,
	valid *Validator,
	jobs *JobStore,
	results resultStore,
	timeout time.Duration,
	maxTries int,
	log *slog.Logger,
) *Worker {
	if poolSize < 1 {
		poolSize = 1
	}
	if maxTries < 1 {
		maxTries = 1
	}
	return &Worker{
		queue:    make(chan task, queueSize),
		llm:      llmClient,
		valid:    valid,
		jobs:     jobs,
		results:  results,
		log:      log,
		timeout:  timeout,
		maxTries: maxTries,
		size:     poolSize,
	}
}

// Bind wires the service back into the worker. The two are mutually dependent:
// the service enqueues, the worker reads corpus state through the service. Being
// explicit about it beats duplicating the assembly logic.
func (w *Worker) Bind(svc *Service) {
	w.svc = svc
}

// msgStopped is the job error when shutdown interrupts or pre-empts a job.
const msgStopped = "the server restarted before this recipe was adapted; please try again"

// finalWriteTimeout bounds the last writes of a job made after shutdown has
// cancelled the worker context.
const finalWriteTimeout = 5 * time.Second

// Start launches the pool. On ctx cancellation each worker fails the jobs still
// queued and exits; the in-flight call is cancelled through the same ctx.
func (w *Worker) Start(ctx context.Context) {
	for i := 0; i < w.size; i++ {
		w.wg.Add(1)
		go func(id int) {
			defer w.wg.Done()
			log := w.log.With(slog.Int("worker", id))
			for {
				select {
				case <-ctx.Done():
					// Failing queued jobs here rather than only in Stop ends their
					// SSE streams before the HTTP server waits for open connections.
					w.dropQueued()
					return
				case t, ok := <-w.queue:
					if !ok {
						return
					}
					w.run(ctx, log, t)
				}
			}
		}(i)
	}
}

// Stop waits for in-flight tasks, then fails whatever is still queued, including
// tasks enqueued while the HTTP server was shutting down. Jobs are not resumed:
// the user presses the button again, and the released key lets the retry through.
func (w *Worker) Stop() {
	w.wg.Wait()
	w.dropQueued()
}

// dropQueued fails every queued task without blocking.
func (w *Worker) dropQueued() {
	for {
		select {
		case t := <-w.queue:
			log := w.log.With(slog.String("job_id", t.Job.ID.String()), slog.String("user_id", t.Job.UserID.String()))
			w.fail(context.Background(), log, t, t.Job, msgStopped, errors.New("the worker stopped before the job started"))
		default:
			return
		}
	}
}

// finalContext lets the last write of a job land after shutdown cancelled ctx.
// Without it the job would stay queued or running until JOB_TTL, its SSE stream
// would not end, and a retry with the same Idempotency-Key would get it back.
func finalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), finalWriteTimeout)
}

// saveFinal stores a job in a terminal state.
func (w *Worker) saveFinal(ctx context.Context, job Job) error {
	ctx, cancel := finalContext(ctx)
	defer cancel()
	return w.jobs.Save(ctx, job)
}

// Enqueue is non-blocking. A full queue is reported as unavailable rather than
// making the HTTP request wait, because the bottleneck is a single GPU and
// honest backpressure is better than a hidden delay.
func (w *Worker) Enqueue(t task) error {
	select {
	case w.queue <- t:
		return nil
	default:
		return fmt.Errorf("%w: the adaptation queue is full", httpx.ErrUnavailable)
	}
}

// run executes one job: call the model, validate, retry on rejection, and fall
// back to the unadapted recipe when every attempt fails.
func (w *Worker) run(ctx context.Context, log *slog.Logger, t task) {
	job := t.Job
	log = log.With(slog.String("job_id", job.ID.String()), slog.String("user_id", job.UserID.String()))

	job.Status = JobRunning
	if err := w.jobs.Save(ctx, job); err != nil {
		log.Error("could not mark job running", slog.Any("error", err))
	}

	ac, err := w.svc.buildAdaptRequest(ctx, t)
	if err != nil {
		w.fail(ctx, log, t, job, "could not prepare the adaptation request", err)
		return
	}

	in := ValidationInput{
		CandidateIDs:  t.CandidateIDs,
		HaveIDs:       ac.HaveIDs,
		DenyAllergens: t.Prefs.EffectiveAllergens(),
		Bases:         ac.Bases,
		BaseLines:     ac.BaseLines,
		Servings:      t.Servings,
	}

	// lastReason is a complete phrase, not a fragment: the fallback message shown
	// to the user must describe what actually happened on the final attempt,
	// which may be a rejection or a failed call. It reaches the client, so upstream
	// error text stays out of it and goes to the log and generation_runs instead.
	var lastReason string
	attempts := 0
	for attempt := 1; attempt <= w.maxTries; attempt++ {
		attempts = attempt
		job.Attempts = attempt
		if err := w.jobs.Save(ctx, job); err != nil {
			log.Warn("could not publish attempt count", slog.Any("error", err))
		}

		callCtx, cancel := context.WithTimeout(ctx, w.timeout)
		started := time.Now()
		resp, err := w.llm.Adapt(callCtx, ac.Request)
		cancel()
		latency := int(time.Since(started).Milliseconds())

		if err != nil {
			// Shutdown cancelled the call. Retrying on a cancelled context cannot
			// succeed, and the model was not at fault.
			if ctx.Err() != nil {
				w.fail(ctx, log, t, job, msgStopped, err)
				return
			}
			w.recordRun(ctx, job, resp.Meta, latency, recipehistory.RunError, err.Error())
			switch {
			// A transport or availability failure is not the model being wrong;
			// retrying it here would just occupy the GPU queue.
			case errors.Is(err, httpx.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
				w.fail(ctx, log, t, job, "the recipe model is busy or unavailable right now; recipe search still works", err)
				return
			// A 400 means llm-service refused our payload: the two services
			// disagree about the contract. That is our bug, and no number of
			// retries or fallbacks will fix it, so it fails loudly.
			case errors.Is(err, ErrRequestRejected):
				w.fail(ctx, log, t, job, "the recipe service rejected our request; this is a bug on our side", err)
				return
			// A 422 means llm-service could not produce schema-valid output for
			// this input at all. Retrying is pointless, so serve the recipe
			// unadapted instead of burning another slow generation.
			case errors.Is(err, ErrModelUnusable):
				w.serveFallback(ctx, log, t, job, ac.HaveIDs,
					"llm-service could not produce a usable answer for this recipe")
				return
			}
			lastReason = "the adaptation call failed"
			log.Warn("adaptation call failed", slog.Int("attempt", attempt), slog.Any("error", err))
			continue
		}

		result, err := w.valid.Validate(ctx, in, resp.Recipe)
		if err != nil {
			if rej, ok := AsRejection(err); ok {
				w.recordRun(ctx, job, resp.Meta, latency, recipehistory.RunRejected, rej.Error())
				lastReason = fmt.Sprintf("the model output was rejected (%s)", rej.Reason)
				log.Warn("model output rejected",
					slog.Int("attempt", attempt),
					slog.String("reason", string(rej.Reason)),
					slog.String("detail", rej.Detail))
				continue
			}
			w.recordRun(ctx, job, resp.Meta, latency, recipehistory.RunError, err.Error())
			w.fail(ctx, log, t, job, "could not validate the model output", err)
			return
		}

		result.ID = uuid.New()
		result.Model = resp.Meta.Model
		w.recordRun(ctx, job, resp.Meta, latency, recipehistory.RunSuccess, "")
		w.persist(ctx, log, job, result)

		job.Status = JobDone
		job.Result = &result
		if err := w.saveFinal(ctx, job); err != nil {
			log.Error("could not store job result", slog.Any("error", err))
		}
		log.Info("adaptation done",
			slog.Int("attempt", attempt),
			slog.Int64("base_recipe_id", result.BaseRecipeID),
			slog.Int("latency_ms", latency))
		return
	}

	// Every attempt failed. Serving the retrieved recipe unadapted is more useful
	// than an error, and the flag tells the client what happened.
	w.serveFallback(ctx, log, t, job, ac.HaveIDs,
		fmt.Sprintf("%s on all %d attempts", lastReason, attempts))
}

// serveFallback returns the retrieved recipe without adaptation. reason is shown
// to the user, so callers pass a complete explanation rather than a fragment.
func (w *Worker) serveFallback(ctx context.Context, log *slog.Logger, t task, job Job, haveIDs []int64, reason string) {
	recipeID := t.CandidateIDs[0]
	result, err := w.svc.fallbackResult(ctx, recipeID, haveIDs, reason)
	if err != nil {
		w.fail(ctx, log, t, job, "the model output was rejected and no fallback recipe could be built", err)
		return
	}
	result.ID = uuid.New()
	w.persist(ctx, log, job, result)

	job.Status = JobDone
	job.Result = &result
	if err := w.saveFinal(ctx, job); err != nil {
		log.Error("could not store fallback result", slog.Any("error", err))
	}
	log.Warn("served unadapted fallback",
		slog.Int64("base_recipe_id", recipeID),
		slog.String("reason", reason))
}

func (w *Worker) persist(ctx context.Context, log *slog.Logger, job Job, result Result) {
	payload, err := json.Marshal(result)
	if err != nil {
		log.Error("could not marshal result for storage", slog.Any("error", err))
		return
	}
	var model *string
	if result.Model != "" {
		model = &result.Model
	}
	record := recipehistory.Entry{
		ID:           result.ID,
		UserID:       job.UserID,
		BaseRecipeID: result.BaseRecipeID,
		Result:       payload,
		Model:        model,
		Adapted:      result.Adapted,
	}
	if err := w.results.SaveAdapted(ctx, record); err != nil {
		// History is secondary; losing it must not fail a job whose result the
		// user is already waiting on.
		log.Error("could not save adapted recipe", slog.Any("error", err))
	}
}

func (w *Worker) recordRun(ctx context.Context, job Job, meta RunMeta, latency int, status, errMsg string) {
	if meta.LatencyMS == 0 {
		meta.LatencyMS = latency
	}
	run := recipehistory.Run{
		ID:            uuid.New(),
		UserID:        job.UserID,
		JobID:         job.ID,
		Model:         meta.Model,
		PromptVersion: meta.PromptVersion,
		LatencyMS:     meta.LatencyMS,
		// Token counts are optional upstream: not every provider reports usage.
		PromptTokens: derefInt(meta.PromptTokens),
		OutputTokens: derefInt(meta.CompletionTokens),
		Retries:      meta.Retries,
		Status:       status,
		Error:        truncate(errMsg, 2000),
	}
	if err := w.results.SaveRun(ctx, run); err != nil {
		w.log.ErrorContext(ctx, "could not record generation run", slog.Any("error", err))
	}
}

func (w *Worker) fail(ctx context.Context, log *slog.Logger, t task, job Job, message string, cause error) {
	// Any step can fail because shutdown cancelled ctx; the user is told the
	// actual reason rather than the symptom of the step that happened to break.
	if ctx.Err() != nil {
		message = msgStopped
	}
	ctx, cancel := finalContext(ctx)
	defer cancel()

	// Release the idempotency key so the user can retry the same action without
	// getting the failed job handed back to them.
	w.jobs.ReleaseIdempotencyKey(ctx, job.UserID, t.IdempotencyKey)

	job.Status = JobFailed
	job.Error = message
	if err := w.jobs.Save(ctx, job); err != nil {
		log.Error("could not mark job failed", slog.Any("error", err))
	}
	log.Error("adaptation job failed", slog.String("message", message), slog.Any("error", cause))
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
