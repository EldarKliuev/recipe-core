package cook

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/catalog"
	"github.com/recipe-ai/core-api/internal/inventory"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/recipe-ai/core-api/internal/user"
)

// Consumer-side interfaces: everything the cook service needs from other
// packages, so it can be unit tested without Postgres, Redis or llm-service.

type retriever interface {
	Coverage(ctx context.Context, q Query) ([]coverageRow, error)
	IsRetrievableFor(ctx context.Context, q Query, recipeID int64) (bool, error)
	HaveIngredientIDs(ctx context.Context, userID uuid.UUID) ([]int64, error)
}

type corpus interface {
	RecipesByIDs(ctx context.Context, ids []int64) (map[int64]catalog.Recipe, error)
	IngredientsForRecipes(ctx context.Context, recipeIDs []int64) (map[int64][]catalog.RecipeIngredient, error)
	Directions(ctx context.Context, recipeIDs []int64) (map[int64][]string, error)
	IngredientsByIDs(ctx context.Context, ids []int64) ([]catalog.Ingredient, error)
}

type prefsReader interface {
	Prefs(ctx context.Context, userID uuid.UUID) (user.Prefs, error)
}

type fridgeReader interface {
	List(ctx context.Context, userID uuid.UUID) ([]inventory.Item, error)
}

type Service struct {
	retrieval retriever
	corpus    corpus
	prefsRepo prefsReader
	fridge    fridgeReader
	jobs      *JobStore
	worker    *Worker
}

func NewService(
	retrieval retriever,
	corpus corpus,
	prefs prefsReader,
	fridge fridgeReader,
	jobs *JobStore,
	worker *Worker,
) *Service {
	return &Service{retrieval: retrieval, corpus: corpus, prefsRepo: prefs, fridge: fridge, jobs: jobs, worker: worker}
}

const (
	defaultMaxMissing = 3
	maxMaxMissing     = 10
	defaultLimit      = 20
	maxLimit          = 50
	// candidateCount is how many recipes are offered to the model when the user
	// has not picked one. The root plan caps this at 3-5: a 7-14B model adapting
	// from a short list is far more reliable than one inventing from scratch.
	candidateCount = 5
)

// Matches is the synchronous, deterministic search path. It must keep working
// when llm-service is down, so nothing here touches the model.
func (s *Service) Matches(ctx context.Context, userID uuid.UUID, req MatchesRequest) (MatchesResponse, error) {
	q, prefs, err := s.buildQuery(ctx, userID, req)
	if err != nil {
		return MatchesResponse{}, err
	}

	fridgeIDs, err := s.retrieval.HaveIngredientIDs(ctx, userID)
	if err != nil {
		return MatchesResponse{}, err
	}

	rows, err := s.retrieval.Coverage(ctx, q)
	if err != nil {
		return MatchesResponse{}, err
	}
	matches, err := s.hydrate(ctx, rows, fridgeIDs)
	if err != nil {
		return MatchesResponse{}, err
	}

	allergens := prefs.EffectiveAllergens()
	if allergens == nil {
		allergens = []string{}
	}
	return MatchesResponse{
		Items:         matches,
		InventorySize: len(fridgeIDs),
		Allergens:     allergens,
	}, nil
}

func (s *Service) buildQuery(ctx context.Context, userID uuid.UUID, req MatchesRequest) (Query, user.Prefs, error) {
	prefs, err := s.prefsRepo.Prefs(ctx, userID)
	if err != nil {
		return Query{}, user.Prefs{}, err
	}

	q := Query{
		UserID:        userID,
		Allergens:     prefs.EffectiveAllergens(),
		DislikedIDs:   prefs.DislikedIngredientIDs,
		MaxMissing:    defaultMaxMissing,
		Limit:         defaultLimit,
		Categories:    req.Categories,
		UseSoonBefore: time.Now().Add(inventory.UseSoonWindow),
	}
	if req.MaxMissing != nil {
		if *req.MaxMissing < 0 || *req.MaxMissing > maxMaxMissing {
			return q, prefs, httpx.Invalidf("max_missing must be between 0 and %d", maxMaxMissing)
		}
		q.MaxMissing = *req.MaxMissing
	}
	if req.MaxSteps != nil {
		if *req.MaxSteps < 1 {
			return q, prefs, httpx.Invalidf("max_steps must be at least 1")
		}
		q.MaxSteps = *req.MaxSteps
	}
	if req.MaxIngredients != nil {
		if *req.MaxIngredients < 1 {
			return q, prefs, httpx.Invalidf("max_ingredients must be at least 1")
		}
		q.MaxIngredients = *req.MaxIngredients
	}
	if req.Limit != nil {
		if *req.Limit < 1 || *req.Limit > maxLimit {
			return q, prefs, httpx.Invalidf("limit must be between 1 and %d", maxLimit)
		}
		q.Limit = *req.Limit
	}
	return q, prefs, nil
}

// hydrate attaches the have/missing ingredient split to ranked coverage rows.
func (s *Service) hydrate(ctx context.Context, rows []coverageRow, haveIDs []int64) ([]Match, error) {
	if len(rows) == 0 {
		return []Match{}, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RecipeID)
	}
	recipes, err := s.corpus.RecipesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	ingredients, err := s.corpus.IngredientsForRecipes(ctx, ids)
	if err != nil {
		return nil, err
	}

	have := make(map[int64]struct{}, len(haveIDs))
	for _, id := range haveIDs {
		have[id] = struct{}{}
	}

	out := make([]Match, 0, len(rows))
	for _, row := range rows {
		rec, ok := recipes[row.RecipeID]
		if !ok {
			continue
		}
		m := Match{
			RecipeID:       rec.ID,
			Title:          rec.Title,
			Category:       rec.Category,
			Subcategory:    rec.Subcategory,
			Description:    rec.Description,
			NumSteps:       rec.NumSteps,
			NumIngredients: rec.NumIngredients,
			TotalCount:     row.TotalCount,
			MissingCount:   row.MissingCount,
			UseSoonCount:   row.UseSoonCount,
			Have:           []Ingredient{},
			Missing:        []Ingredient{},
		}
		for _, ri := range ingredients[rec.ID] {
			entry := Ingredient{
				IngredientID:  ri.IngredientID,
				CanonicalName: ri.CanonicalName,
				Qty:           ri.Qty,
				Unit:          ri.Unit,
			}
			if _, ok := have[ri.IngredientID]; ok {
				m.Have = append(m.Have, entry)
			} else {
				m.Missing = append(m.Missing, entry)
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// Adapt enqueues an adaptation job and returns immediately. The HTTP request
// never waits for the model.
//
// An Idempotency-Key is mandatory: generations are slow and serialised behind one
// GPU, so a double-submitted form must not consume the queue twice.
func (s *Service) Adapt(ctx context.Context, userID uuid.UUID, idemKey string, req AdaptRequest) (Job, error) {
	idemKey = strings.TrimSpace(idemKey)
	if idemKey == "" {
		return Job{}, httpx.Invalidf("the Idempotency-Key header is required")
	}
	if len(idemKey) > 200 {
		return Job{}, httpx.Invalidf("Idempotency-Key must be at most 200 characters")
	}
	if req.Servings != nil && (*req.Servings < 1 || *req.Servings > 12) {
		return Job{}, httpx.Invalidf("servings must be between 1 and 12")
	}
	if len(req.Notes) > 500 {
		return Job{}, httpx.Invalidf("notes must be at most 500 characters")
	}

	q, prefs, err := s.buildQuery(ctx, userID, MatchesRequest{})
	if err != nil {
		return Job{}, err
	}

	candidateIDs, err := s.candidates(ctx, q, req.RecipeID)
	if err != nil {
		return Job{}, err
	}

	jobID, fresh, err := s.jobs.ClaimIdempotencyKey(ctx, userID, idemKey, uuid.New())
	if err != nil {
		return Job{}, err
	}
	if !fresh {
		// A replayed request returns the original job rather than starting a
		// second generation on a single-GPU model.
		return s.Job(ctx, userID, jobID)
	}

	job := Job{
		ID:        jobID,
		UserID:    userID,
		RecipeID:  req.RecipeID,
		Status:    JobQueued,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.jobs.Save(ctx, job); err != nil {
		s.jobs.ReleaseIdempotencyKey(ctx, userID, idemKey)
		return Job{}, err
	}

	task := task{
		Job:            job,
		IdempotencyKey: idemKey,
		CandidateIDs:   candidateIDs,
		Prefs:          prefs,
		Servings:       req.Servings,
		Notes:          req.Notes,
	}
	if err := s.worker.Enqueue(task); err != nil {
		s.jobs.ReleaseIdempotencyKey(ctx, userID, idemKey)
		job.Status = JobFailed
		job.Error = "the adaptation queue is full, please try again shortly"
		_ = s.jobs.Save(ctx, job)
		return Job{}, err
	}
	return job, nil
}

// candidates resolves which recipes the model may choose from. A recipe the user
// could not have been offered is rejected here rather than in the validator, so
// the user gets a 400 instead of a failed job.
//
// A chosen recipe is checked against the widest max_missing search accepts:
// AdaptRequest does not carry the max_missing of the search that showed it, so
// the default would refuse recipes shown with a higher limit. Allergens and
// dislikes are still enforced in full.
func (s *Service) candidates(ctx context.Context, q Query, requested int64) ([]int64, error) {
	if requested > 0 {
		q.MaxMissing = maxMaxMissing
		ok, err := s.retrieval.IsRetrievableFor(ctx, q, requested)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, httpx.Invalidf(
				"recipe %d is not among your matches; it either needs more than %d missing ingredients or conflicts with your allergens or disliked ingredients",
				requested, maxMaxMissing)
		}
		return []int64{requested}, nil
	}

	q.Limit = candidateCount
	rows, err := s.retrieval.Coverage(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, httpx.Invalidf("no recipes match your fridge yet; add more ingredients or raise max_missing")
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RecipeID)
	}
	return ids, nil
}

func (s *Service) Job(ctx context.Context, userID, jobID uuid.UUID) (Job, error) {
	job, err := s.jobs.Get(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if job.UserID != userID {
		// Report as not found so job ids are not probeable across accounts.
		return Job{}, fmt.Errorf("%w: job %s", httpx.ErrNotFound, jobID)
	}
	return job, nil
}

// StreamJob yields job snapshots until a terminal status is reached or ctx ends.
// It combines pub/sub with a slow poll so a dropped message cannot leave the
// client hanging forever.
func (s *Service) StreamJob(ctx context.Context, userID, jobID uuid.UUID, emit func(Job) error) error {
	job, err := s.Job(ctx, userID, jobID)
	if err != nil {
		return err
	}

	events, cancel, err := s.jobs.Subscribe(ctx, jobID)
	if err != nil {
		return err
	}
	defer cancel()

	// Re-read after subscribing: the job may have completed between the first
	// read and the subscription being established.
	if job, err = s.Job(ctx, userID, jobID); err != nil {
		return err
	}
	if err := emit(job); err != nil {
		return err
	}
	if job.Status.Terminal() {
		return nil
	}

	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	lastStatus := job.Status

	for {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-events:
			if !ok {
				return nil
			}
			if update.UserID != userID {
				continue
			}
			if err := emit(update); err != nil {
				return err
			}
			if update.Status.Terminal() {
				return nil
			}
			lastStatus = update.Status
		case <-poll.C:
			current, err := s.Job(ctx, userID, jobID)
			if err != nil {
				return err
			}
			if current.Status == lastStatus && !current.Status.Terminal() {
				continue
			}
			if err := emit(current); err != nil {
				return err
			}
			if current.Status.Terminal() {
				return nil
			}
			lastStatus = current.Status
		}
	}
}

// adaptContext is everything the worker needs after the request is assembled: the
// payload itself plus the corpus state the validator judges the answer against.
type adaptContext struct {
	Request     LLMAdaptRequest
	Bases       map[int64]catalog.Recipe
	BaseLines   map[int64][]catalog.RecipeIngredient
	HaveIDs     []int64
	MaxRecipes  int
	RequestedID int64
}

// llmMaxRecipes mirrors the max_length on llm-service's recipes field. Sending
// more is a 400, so the candidate list is capped here rather than discovered at
// the far end.
const llmMaxRecipes = 5

// buildAdaptRequest assembles the llm-service payload: the retrieved recipes with
// their verbatim corpus directions for paraphrasing, the fridge, and prefs.
func (s *Service) buildAdaptRequest(ctx context.Context, t task) (adaptContext, error) {
	recipes, err := s.corpus.RecipesByIDs(ctx, t.CandidateIDs)
	if err != nil {
		return adaptContext{}, err
	}
	ingredients, err := s.corpus.IngredientsForRecipes(ctx, t.CandidateIDs)
	if err != nil {
		return adaptContext{}, err
	}
	directions, err := s.corpus.Directions(ctx, t.CandidateIDs)
	if err != nil {
		return adaptContext{}, err
	}
	haveIDs, err := s.retrieval.HaveIngredientIDs(ctx, t.Job.UserID)
	if err != nil {
		return adaptContext{}, err
	}
	fridge, err := s.fridge.List(ctx, t.Job.UserID)
	if err != nil {
		return adaptContext{}, err
	}

	offered := make([]LLMRetrievedRecipe, 0, len(t.CandidateIDs))
	for _, id := range t.CandidateIDs {
		rec, ok := recipes[id]
		if !ok {
			continue
		}
		if len(offered) == llmMaxRecipes {
			break
		}
		lines := make([]string, 0, len(ingredients[id]))
		for _, ri := range ingredients[id] {
			lines = append(lines, ingredientLine(ri))
		}
		// Every list is initialised empty rather than left nil: llm-service is
		// FastAPI, and a null where it declares a list is rejected by pydantic
		// before the model is ever asked anything.
		offered = append(offered, LLMRetrievedRecipe{
			ID:          rec.ID,
			Title:       rec.Title,
			Ingredients: lines,
			Steps:       emptyIfNil(directions[id]),
		})
	}
	if len(offered) == 0 {
		return adaptContext{}, fmt.Errorf("none of the candidate recipes %v exist in the corpus", t.CandidateIDs)
	}

	items := make([]string, 0, len(fridge))
	for _, it := range fridge {
		items = append(items, inventoryLine(it))
	}
	if len(items) == 0 {
		// llm-service requires a non-empty inventory, and adapting to an empty
		// fridge is meaningless anyway.
		return adaptContext{}, httpx.Invalidf("your fridge is empty, so there is nothing to adapt a recipe to")
	}

	dislikes, err := s.dislikeNames(ctx, t.Prefs.DislikedIngredientIDs)
	if err != nil {
		return adaptContext{}, err
	}

	return adaptContext{
		Request: LLMAdaptRequest{
			Recipes:   offered,
			Inventory: items,
			Prefs: LLMPrefs{
				Allergies: emptyIfNil(t.Prefs.EffectiveAllergens()),
				Diet:      dietHint(t.Prefs, t.Notes),
				Dislikes:  emptyIfNil(dislikes),
				Servings:  t.Servings,
			},
		},
		Bases:     recipes,
		BaseLines: ingredients,
		HaveIDs:   haveIDs,
	}, nil
}

// ingredientLine renders a corpus ingredient for the prompt. The raw corpus text
// already reads like a recipe line, so it is preferred over rebuilding one.
func ingredientLine(ri catalog.RecipeIngredient) string {
	if text := strings.TrimSpace(ri.RawText); text != "" {
		return text
	}
	return strings.TrimSpace(strings.Join([]string{amount(ri.Qty, ri.Unit), ri.CanonicalName}, " "))
}

// inventoryLine folds the amount and the near-expiry hint into one string,
// because llm-service takes the inventory as a flat list of strings.
func inventoryLine(it inventory.Item) string {
	line := strings.TrimSpace(strings.Join([]string{amount(it.Qty, it.Unit), it.CanonicalName}, " "))
	if it.UseSoon {
		line += " (use soon)"
	}
	return line
}

func amount(qty *float64, unit *string) string {
	var parts []string
	if qty != nil {
		parts = append(parts, strconv.FormatFloat(*qty, 'f', -1, 64))
	}
	if unit != nil && strings.TrimSpace(*unit) != "" {
		parts = append(parts, strings.TrimSpace(*unit))
	}
	return strings.Join(parts, " ")
}

// dietHint packs diets, cuisines and the user's free-text notes into the single
// free-text `diet` field, which is the only place llm-service will read them:
// its schema has no cuisines or notes field and ignores anything extra.
func dietHint(prefs user.Prefs, notes string) *string {
	parts := make([]string, 0, 3)
	if len(prefs.Diets) > 0 {
		parts = append(parts, strings.Join(prefs.Diets, ", "))
	}
	if len(prefs.Cuisines) > 0 {
		parts = append(parts, "prefers "+strings.Join(prefs.Cuisines, ", ")+" cuisine")
	}
	if n := strings.TrimSpace(notes); n != "" {
		parts = append(parts, n)
	}
	if len(parts) == 0 {
		return nil
	}
	hint := strings.Join(parts, "; ")
	return &hint
}

// emptyIfNil keeps nil slices out of the outbound JSON, where they would encode
// as null and fail llm-service's schema validation.
func emptyIfNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

func (s *Service) dislikeNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	ings, err := s.corpus.IngredientsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ings))
	for _, ing := range ings {
		names = append(names, ing.CanonicalName)
	}
	return names, nil
}

// fallbackResult builds the unadapted answer used after every model attempt has
// been rejected. It carries the corpus ingredient split but no steps, because
// verbatim corpus directions must never be served to a client.
func (s *Service) fallbackResult(ctx context.Context, recipeID int64, haveIDs []int64, reason string) (Result, error) {
	recipes, err := s.corpus.RecipesByIDs(ctx, []int64{recipeID})
	if err != nil {
		return Result{}, err
	}
	rec, ok := recipes[recipeID]
	if !ok {
		return Result{}, fmt.Errorf("%w: recipe %d", httpx.ErrNotFound, recipeID)
	}
	ingredients, err := s.corpus.IngredientsForRecipes(ctx, []int64{recipeID})
	if err != nil {
		return Result{}, err
	}

	res := Result{
		BaseRecipeID:   recipeID,
		Title:          rec.Title,
		Adapted:        false,
		FallbackReason: reason,
		Used:           []Ingredient{},
		Missing:        []Ingredient{},
		Substitutions:  []Substitution{},
		Steps:          []string{},
		Base:           rec,
	}
	for _, ri := range ingredients[recipeID] {
		entry := Ingredient{IngredientID: ri.IngredientID, CanonicalName: ri.CanonicalName, Qty: ri.Qty, Unit: ri.Unit}
		if slices.Contains(haveIDs, ri.IngredientID) {
			res.Used = append(res.Used, entry)
		} else {
			res.Missing = append(res.Missing, entry)
		}
	}
	return res, nil
}
