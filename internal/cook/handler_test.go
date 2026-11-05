package cook

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/authctx"
)

// flushHookRecorder runs onFlush at the first flush, which the SSE handler does
// right after sending headers. That is the moment a failure stops being
// expressible as an HTTP status.
type flushHookRecorder struct {
	*httptest.ResponseRecorder
	onFlush func()
}

func (r *flushHookRecorder) Flush() {
	if r.onFlush != nil {
		r.onFlush()
		r.onFlush = nil
	}
	r.ResponseRecorder.Flush()
}

// captureLogs points the default logger at a buffer for the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// streamRunningJob opens the SSE endpoint for a job that has not finished.
func streamRunningJob(t *testing.T, ctx context.Context, h *harness, onFlush func()) string {
	t.Helper()
	job := Job{ID: uuid.New(), UserID: h.userID, Status: JobRunning, CreatedAt: time.Now().UTC()}
	if err := h.jobs.Save(ctx, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/jobs/"+job.ID.String(), nil)
	req = req.WithContext(authctx.With(ctx, h.userID))
	req.Header.Set("Accept", "text/event-stream")

	rec := &flushHookRecorder{ResponseRecorder: httptest.NewRecorder(), onFlush: onFlush}
	NewHandler(h.svc).Routes().ServeHTTP(rec, req)
	return rec.Body.String()
}

// The stream reaches the client, so an internal error stays in the log and the
// client gets the same frame shape with a fixed message.
func TestJobStreamDoesNotExposeInternalErrors(t *testing.T) {
	logs := captureLogs(t)
	h := newHarness(t, nil, 1)

	body := streamRunningJob(t, context.Background(), h, func() { _ = h.jobs.client.Close() })

	want := "event: error\ndata: {\"message\":\"could not read the job status\"}\n\n"
	if !strings.Contains(body, want) {
		t.Errorf("body = %q, want the fixed error frame", body)
	}
	if strings.Contains(body, "redis") {
		t.Errorf("body = %q, must not contain the internal error", body)
	}
	if !strings.Contains(logs.String(), "client is closed") {
		t.Errorf("logs = %q, want the internal error kept for diagnosis", logs.String())
	}
}

// A client that went away is not a failure: nobody can read the frame and it
// must not raise an error in the log.
func TestJobStreamStaysQuietWhenTheClientDisconnects(t *testing.T) {
	logs := captureLogs(t)
	h := newHarness(t, nil, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := streamRunningJob(t, ctx, h, cancel)

	if strings.Contains(body, "event: error") {
		t.Errorf("body = %q, want no error frame for a disconnected client", body)
	}
	if strings.Contains(logs.String(), "job stream failed") {
		t.Errorf("logs = %q, want no error for a disconnected client", logs.String())
	}
}
