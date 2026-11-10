package cook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

// ErrModelUnusable reports a 422 from llm-service: it could not produce output
// matching its own schema for this input. Retrying will not help, so the job goes
// straight to the unadapted fallback.
var ErrModelUnusable = errors.New("llm-service could not produce a usable answer")

// ErrRequestRejected reports a 400: llm-service refused the payload itself. That
// is a contract bug on this side, not a bad generation, so it must fail loudly
// instead of being retried or hidden behind a fallback that blames the model.
var ErrRequestRejected = errors.New("llm-service rejected the request payload")

type LLMClient struct {
	baseURL string
	http    *http.Client
}

// NewClient builds the llm-service client. The timeout is generous by design:
// the model is local and slow, so 180s is the configured default rather than an
// anomaly. Adaptation never happens inside a user-facing HTTP request, so a long
// timeout costs nothing but a worker slot.
func NewLLMClient(baseURL string, timeout time.Duration) *LLMClient {
	return &LLMClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Adapt calls POST /v1/adapt. A 503 from llm-service means the single GPU is
// busy; it is surfaced as ErrUnavailable so the job fails with a retryable
// message rather than looking like a bug.
func (c *LLMClient) Adapt(ctx context.Context, req LLMAdaptRequest) (LLMAdaptResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return LLMAdaptResponse{}, fmt.Errorf("marshal adapt request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/adapt", bytes.NewReader(body))
	if err != nil {
		return LLMAdaptResponse{}, fmt.Errorf("build adapt request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return LLMAdaptResponse{}, fmt.Errorf("%w: llm-service unreachable: %v", httpx.ErrUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		detail, meta := readErrorDetail(resp.Body)
		switch resp.StatusCode {
		case http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusGatewayTimeout:
			// 503 is a saturated GPU or a down provider, 504 is a provider that
			// took too long. Both are availability problems, not wrong answers,
			// so they surface as unavailable and are never retried here.
			return LLMAdaptResponse{}, fmt.Errorf("%w: llm-service is busy or timed out: %s", httpx.ErrUnavailable, detail)
		case http.StatusUnprocessableEntity:
			// The model did answer, so the run has metadata worth recording.
			return LLMAdaptResponse{Meta: meta}, fmt.Errorf("%w: %s", ErrModelUnusable, detail)
		case http.StatusBadRequest:
			return LLMAdaptResponse{}, fmt.Errorf("%w: %s", ErrRequestRejected, detail)
		default:
			return LLMAdaptResponse{}, fmt.Errorf("llm-service returned %d: %s", resp.StatusCode, detail)
		}
	}

	var out LLMAdaptResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return LLMAdaptResponse{}, fmt.Errorf("decode adapt response: %w", err)
	}
	return out, nil
}

// There is deliberately no health probe here. core-api's own readiness must not
// depend on llm-service: retrieval is the main path and has to keep working with
// the model down, so a probe would only create a false reason to report this
// service as unready.

// readErrorDetail summarises an llm-service error body and returns the run
// metadata it carries. That service wraps errors as {"error": {"code",
// "message", "detail"}}; plain FastAPI {"detail": ...} is accepted too, since
// validation errors raised before its handlers use that shape.
//
// The detail on a 400 is the whole pydantic error list, which is far too long to
// put in a user-facing fallback reason, so only the code and message are kept.
// On a 422 the detail is an object whose "meta" has the same shape as RunMeta in
// a successful response; anywhere else the metadata comes back empty.
func readErrorDetail(body io.Reader) (string, RunMeta) {
	raw, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil || len(raw) == 0 {
		return "no detail", RunMeta{}
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			// Raw because its shape depends on the status: a list on a 400.
			Detail json.RawMessage `json:"detail"`
		} `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		if envelope.Error.Code != "" {
			var detail struct {
				Meta RunMeta `json:"meta"`
			}
			// Not an object, or no meta: the run simply has no metadata.
			_ = json.Unmarshal(envelope.Error.Detail, &detail)
			if envelope.Error.Message == "" {
				return envelope.Error.Code, detail.Meta
			}
			return envelope.Error.Code + ": " + envelope.Error.Message, detail.Meta
		}
		if len(envelope.Detail) > 0 {
			return truncate(strings.Trim(string(envelope.Detail), `"`), 300), RunMeta{}
		}
	}
	return truncate(string(raw), 300), RunMeta{}
}
