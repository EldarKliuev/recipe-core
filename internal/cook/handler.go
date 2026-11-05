package cook

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/authctx"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/matches", h.matches)
	r.Post("/adapt", h.adapt)
	r.Get("/jobs/{jobID}", h.job)
	return r
}

func (h *Handler) matches(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// An empty body is a valid "use my defaults" request.
	req := MatchesRequest{}
	if r.ContentLength > 0 {
		if err := httpx.Decode(r, &req); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}
	resp, err := h.svc.Matches(r.Context(), userID, req)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

type adaptResponse struct {
	JobID  uuid.UUID `json:"job_id"`
	Status JobStatus `json:"status"`
	// StreamURL is where the client subscribes for progress.
	StreamURL string `json:"stream_url"`
}

func (h *Handler) adapt(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req AdaptRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	job, err := h.svc.Adapt(r.Context(), userID, r.Header.Get("Idempotency-Key"), req)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, adaptResponse{
		JobID:     job.ID,
		Status:    job.Status,
		StreamURL: fmt.Sprintf("/api/v1/cook/jobs/%s", job.ID),
	})
}

// job serves job state. It streams Server-Sent Events when the client asks for
// text/event-stream and returns a plain JSON snapshot otherwise, which keeps the
// endpoint usable from curl and from the mobile client later.
func (h *Handler) job(w http.ResponseWriter, r *http.Request) {
	userID, err := authctx.UserID(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		httpx.Fail(w, r, httpx.Invalidf("jobID must be a uuid"))
		return
	}

	if !wantsSSE(r) {
		job, err := h.svc.Job(r.Context(), userID, jobID)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.JSON(w, http.StatusOK, job)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.Fail(w, r, fmt.Errorf("streaming is not supported by this server"))
		return
	}

	// Probe before writing SSE headers so that auth and ownership errors are
	// still expressible as a normal HTTP status.
	if _, err := h.svc.Job(r.Context(), userID, jobID); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable proxy buffering, which otherwise defeats the whole point of SSE.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(job Job) error {
		payload, err := json.Marshal(job)
		if err != nil {
			return fmt.Errorf("marshal job event: %w", err)
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", job.Status, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := h.svc.StreamJob(r.Context(), userID, jobID, emit); err != nil {
		// A client that went away surfaces here too, and there is nobody to tell.
		if r.Context().Err() != nil {
			return
		}
		slog.ErrorContext(r.Context(), "job stream failed",
			slog.String("job_id", jobID.String()),
			slog.Any("error", err))
		// Headers are already sent, so report the failure inside the stream.
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"message\":\"could not read the job status\"}\n\n")
		flusher.Flush()
	}
}

func wantsSSE(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}
