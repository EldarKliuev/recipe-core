package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/recipe-ai/core-api/internal/platform/authctx"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

// Middleware rejects requests without a valid access token and puts the caller
// id into the request context.
func Middleware(tokens *TokenIssuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				httpx.Fail(w, r, fmt.Errorf("%w: missing bearer token", httpx.ErrUnauthorized))
				return
			}
			userID, err := tokens.ParseAccessToken(raw)
			if err != nil {
				httpx.Fail(w, r, fmt.Errorf("%w: invalid access token", httpx.ErrUnauthorized))
				return
			}
			next.ServeHTTP(w, r.WithContext(authctx.With(r.Context(), userID)))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// counter is the Redis surface the rate limiter needs.
type counter interface {
	Incr(ctx context.Context, key string) *IntResult
	Expire(ctx context.Context, key string, ttl time.Duration) error
}

// RateLimiter is a fixed-window per-IP limiter for the unauthenticated auth
// endpoints. Redis failures fail open: an unavailable cache must not lock users
// out of logging in.
type RateLimiter struct {
	counter counter
	limit   int
	window  time.Duration
	log     *slog.Logger
}

func NewRateLimiter(c counter, limit int, window time.Duration, log *slog.Logger) *RateLimiter {
	return &RateLimiter{counter: c, limit: limit, window: window, log: log}
}

func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := fmt.Sprintf("ratelimit:auth:%s:%d", clientIP(r), time.Now().UnixNano()/int64(l.window))
		res := l.counter.Incr(r.Context(), key)
		if res.Err != nil {
			l.log.WarnContext(r.Context(), "rate limiter unavailable, allowing request",
				slog.Any("error", res.Err))
			next.ServeHTTP(w, r)
			return
		}
		if res.Value == 1 {
			if err := l.counter.Expire(r.Context(), key, l.window); err != nil {
				l.log.WarnContext(r.Context(), "rate limiter expire failed", slog.Any("error", err))
			}
		}
		if res.Value > int64(l.limit) {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(l.window.Seconds())))
			httpx.Fail(w, r, fmt.Errorf("%w: too many attempts, slow down", httpx.ErrTooManyTries))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IntResult keeps the limiter independent of the Redis client type.
type IntResult struct {
	Value int64
	Err   error
}

func clientIP(r *http.Request) string {
	// Only the leftmost X-Forwarded-For entry is meaningful, and only because
	// this service sits behind a trusted proxy in deployment.
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
