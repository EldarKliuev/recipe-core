package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/authctx"
)

func TestMiddlewarePutsCallerInContext(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	userID := uuid.New()
	token, _, err := issuer.AccessToken(userID)
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}

	var seen uuid.UUID
	handler := Middleware(issuer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = authctx.UserID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/prefs", nil)
	req.Header.Set("Authorization", "bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != userID {
		t.Errorf("context user = %s, want %s", seen, userID)
	}
}

func TestMiddlewareRejectsMissingAndMalformedTokens(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	handler := Middleware(issuer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran for an unauthenticated request")
	}))

	for name, header := range map[string]string{
		"missing":      "",
		"wrong scheme": "Basic abc",
		"empty token":  "Bearer ",
		"garbage":      "Bearer not.a.jwt",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/me/prefs", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// fakeCounter lets the limiter be exercised without Redis.
type fakeCounter struct {
	counts map[string]int64
	err    error
}

func (f *fakeCounter) Incr(_ context.Context, key string) *IntResult {
	if f.err != nil {
		return &IntResult{Err: f.err}
	}
	f.counts[key]++
	return &IntResult{Value: f.counts[key]}
}

func (f *fakeCounter) Expire(context.Context, string, time.Duration) error { return f.err }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRateLimiterBlocksAfterTheLimit(t *testing.T) {
	counter := &fakeCounter{counts: map[string]int64{}}
	limiter := NewRateLimiter(counter, 3, time.Minute, quietLogger())
	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 1; i <= 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, loginRequest("203.0.113.7:1234"))
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loginRequest("203.0.113.7:1234"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("fourth attempt status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header is missing from the 429")
	}
}

func TestRateLimiterCountsPerClient(t *testing.T) {
	counter := &fakeCounter{counts: map[string]int64{}}
	limiter := NewRateLimiter(counter, 1, time.Minute, quietLogger())
	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, loginRequest("203.0.113.7:1234"))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, loginRequest("198.51.100.2:5678"))

	if second.Code != http.StatusOK {
		t.Errorf("a different client got %d, want 200; the window is not per-IP", second.Code)
	}
}

// Redis being down must not lock users out of logging in.
func TestRateLimiterFailsOpenWhenRedisIsDown(t *testing.T) {
	counter := &fakeCounter{counts: map[string]int64{}, err: errors.New("redis is down")}
	limiter := NewRateLimiter(counter, 1, time.Minute, quietLogger())

	called := false
	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loginRequest("203.0.113.7:1234"))
	if !called {
		t.Error("request was blocked while Redis was unavailable, want fail-open")
	}
}

func loginRequest(remoteAddr string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.RemoteAddr = remoteAddr
	return req
}
