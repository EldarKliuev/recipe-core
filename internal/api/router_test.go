package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func probe(t *testing.T, ready func(context.Context) error) (*httptest.ResponseRecorder, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	router := NewRouter(Deps{
		Log:   slog.New(slog.NewTextHandler(&logs, nil)),
		Ready: ready,
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec, &logs
}

func TestReadyzReportsReady(t *testing.T) {
	rec, _ := probe(t, func(context.Context) error { return nil })

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ready" {
		t.Fatalf("body = %v, want status ready", body)
	}
}

func TestReadyzDoesNotExposeTheDependencyError(t *testing.T) {
	cause := errors.New("postgres: failed to connect to `host=db.internal user=recipes database=recipes`: dial tcp 10.0.3.7:5432: connect: connection refused")
	rec, logs := probe(t, func(context.Context) error { return cause })

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body) != 1 || body["status"] != "unavailable" {
		t.Fatalf("body = %v, want only status unavailable", body)
	}
	for _, leak := range []string{"postgres", "db.internal", "10.0.3.7", "5432"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response body leaks %q: %s", leak, rec.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "10.0.3.7:5432") {
		t.Errorf("cause is missing from the log: %s", logs.String())
	}
}
