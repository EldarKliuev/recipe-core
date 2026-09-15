// The outbound contract to service 2, llm-service. The interface lives in this
// package (the consumer); LLMClient is its HTTP implementation. Nothing here
// knows about prompts, JSON schemas, model names or GPU concurrency: those
// belong to llm-service.
//
// Contract: POST {LLM_SERVICE_URL}/v1/adapt
//
// core-api sends 3-5 retrieved recipes plus the fridge and prefs; llm-service
// returns one adapted recipe. Division of responsibility for answer quality:
// llm-service guarantees the *form* (valid JSON matching its schema), core-api
// guarantees the *meaning* (ingredients really are in the fridge, allergens are
// not violated, base_recipe_id is one of the candidates we sent).
//
// The shapes below mirror app/domain/schemas.py in llm-service, which owns them.
// It declares extra="ignore", so a field we invent here is silently dropped
// rather than rejected: anything core-api wants the model to see has to fit one
// of the fields that service actually declares.
package cook

// LLMAdaptRequest is the request body of POST /v1/adapt.
type LLMAdaptRequest struct {
	// Recipes is the retrieval set, best first. llm-service accepts 1-5 and must
	// echo the id of the one it picked back as base_recipe_id.
	Recipes []LLMRetrievedRecipe `json:"recipes"`
	// Inventory is the fridge as free text, one line per item, and must be
	// non-empty. It is a flat list of strings on the far side, so amounts and the
	// use-soon hint are folded into each line rather than sent as fields.
	Inventory []string `json:"inventory"`
	Prefs     LLMPrefs `json:"prefs"`
}

// LLMRetrievedRecipe is one retrieved recipe offered to the model. Steps are the
// verbatim corpus directions: the model must paraphrase them, and core-api never
// serves them to clients directly.
type LLMRetrievedRecipe struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	// Ingredients are the raw corpus ingredient lines ("2 tbsp soy sauce").
	Ingredients []string `json:"ingredients"`
	Steps       []string `json:"steps"`
}

// LLMPrefs are hints for the prompt only. The hard allergen deny-list is
// core-api's to enforce, in SQL and again in the validator.
type LLMPrefs struct {
	Allergies []string `json:"allergies"`
	// Diet is a single free-text field on the far side, so multiple diets are
	// joined into it rather than dropped.
	Diet     *string  `json:"diet"`
	Dislikes []string `json:"dislikes"`
	Servings *int     `json:"servings"`
}

// LLMAdaptResponse is the response body of POST /v1/adapt.
type LLMAdaptResponse struct {
	Recipe LLMAdaptedRecipe `json:"recipe"`
	Meta   RunMeta          `json:"meta"`
}

// LLMAdaptedRecipe is the model output, already schema-valid. Its semantic
// validity is core-api's problem. Ingredients come back as bare names with no
// amounts, so quantities in the result are taken from the corpus row rather than
// from anything the model said.
type LLMAdaptedRecipe struct {
	BaseRecipeID       int64             `json:"base_recipe_id"`
	Title              string            `json:"title"`
	Steps              []string          `json:"steps"`
	UsedIngredients    []string          `json:"used_ingredients"`
	MissingIngredients []string          `json:"missing_ingredients"`
	Substitutions      []LLMSubstitution `json:"substitutions"`
}

type LLMSubstitution struct {
	Missing     string `json:"missing"`
	ReplaceWith string `json:"replace_with"`
	Note        string `json:"note"`
}

// RunMeta is the per-call telemetry core-api persists in generation_runs. Token
// counts are nullable on the far side: not every provider reports usage.
type RunMeta struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version"`
	LatencyMS     int    `json:"latency_ms"`
	QueueWaitMS   int    `json:"queue_wait_ms"`
	// Retries counts llm-service's internal schema-repair attempts, not
	// core-api's validation attempts.
	Retries          int  `json:"retries"`
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
}
