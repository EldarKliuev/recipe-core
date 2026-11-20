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
	handler := Middleware(issuer, knownUsers{userID: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	handler := Middleware(issuer, knownUsers{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// knownUsers is the set of accounts that still exist.
type knownUsers map[uuid.UUID]bool

func (k knownUsers) UserExists(_ context.Context, id uuid.UUID) (bool, error) {
	return k[id], nil
}

type brokenUsers struct{}

func (brokenUsers) UserExists(context.Context, uuid.UUID) (bool, error) {
	return false, errors.New("postgres is down")
}

// Access tokens outlive a deleted account; the middleware must turn them away
// before they reach handlers.
func TestMiddlewareRejectsATokenOfADeletedUser(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	token, _, err := issuer.AccessToken(uuid.New())
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	handler := Middleware(issuer, knownUsers{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran for a deleted user")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/prefs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// A failed lookup is not proof the user is gone: it must not log them out with a
// 401, and it must not let the request through unchecked either.
func TestMiddlewareFailsClosedWithoutLoggingOutWhenTheLookupFails(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	token, _, err := issuer.AccessToken(uuid.New())
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	handler := Middleware(issuer, brokenUsers{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran although the account could not be checked")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/prefs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
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
	limiter := NewRateLimiter(counter, 3, time.Minute, 0, quietLogger())
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
	limiter := NewRateLimiter(counter, 1, time.Minute, 0, quietLogger())
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
	limiter := NewRateLimiter(counter, 1, time.Minute, 0, quietLogger())

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

// Without a trusted proxy X-Forwarded-For is written by the client, so changing
// it must not give a fresh window.
func TestRateLimiterIgnoresForwardedForWithoutATrustedProxy(t *testing.T) {
	codes := attempts(0, "203.0.113.7:1234", "198.51.100.1", "198.51.100.2")
	if codes[1] != http.StatusTooManyRequests {
		t.Errorf("second attempt with a new X-Forwarded-For got %d, want 429", codes[1])
	}
}

// Behind nginx ($proxy_add_x_forwarded_for) the client can still prepend
// anything; only the entry the proxy appended identifies it.
func TestRateLimiterIgnoresClientWrittenEntriesBehindAProxy(t *testing.T) {
	codes := attempts(1, "172.18.0.5:40000",
		"198.51.100.1, 203.0.113.7", "198.51.100.2, 203.0.113.7")
	if codes[1] != http.StatusTooManyRequests {
		t.Errorf("second attempt with a new spoofed entry got %d, want 429", codes[1])
	}
}

func TestRateLimiterCountsClientsBehindAProxySeparately(t *testing.T) {
	codes := attempts(1, "172.18.0.5:40000", "203.0.113.7", "198.51.100.2")
	if codes[1] != http.StatusOK {
		t.Errorf("a different client behind the proxy got %d, want 200; all clients share the proxy's window", codes[1])
	}
}

// A header shorter than the number of trusted proxies did not pass through all
// of them, so it is entirely client-written and the peer address is counted.
func TestRateLimiterIgnoresAHeaderShorterThanTheTrustedHops(t *testing.T) {
	codes := attempts(2, "172.18.0.5:40000", "198.51.100.1", "198.51.100.2")
	if codes[1] != http.StatusTooManyRequests {
		t.Errorf("second attempt with a new short header got %d, want 429", codes[1])
	}
}

// attempts sends one login per X-Forwarded-For value from the same peer through
// a limiter that allows a single attempt, and returns the status codes.
func attempts(trustedHops int, remoteAddr string, forwardedFor ...string) []int {
	limiter := NewRateLimiter(&fakeCounter{counts: map[string]int64{}}, 1, time.Minute, trustedHops, quietLogger())
	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	codes := make([]int, 0, len(forwardedFor))
	for _, fwd := range forwardedFor {
		req := loginRequest(remoteAddr)
		req.Header.Set("X-Forwarded-For", fwd)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	return codes
}

func loginRequest(remoteAddr string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.RemoteAddr = remoteAddr
	return req
}
