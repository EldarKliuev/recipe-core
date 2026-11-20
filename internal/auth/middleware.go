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
	// trustedHops is how many reverse proxies in front of the service append to
	// X-Forwarded-For; see clientIP.
	trustedHops int
	log         *slog.Logger
}

func NewRateLimiter(c counter, limit int, window time.Duration, trustedHops int, log *slog.Logger) *RateLimiter {
	return &RateLimiter{counter: c, limit: limit, window: window, trustedHops: trustedHops, log: log}
}

func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := fmt.Sprintf("ratelimit:auth:%s:%d", clientIP(r, l.trustedHops), time.Now().UnixNano()/int64(l.window))
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

// clientIP returns the address the limiter counts attempts against. Every
// X-Forwarded-For entry except those appended by our own proxies is written by
// the client, so with trustedHops proxies in front the client is the
// trustedHops-th entry from the right (nginx's $proxy_add_x_forwarded_for appends
// the peer it saw). With no trusted proxies the header is ignored entirely. A
// header shorter than trustedHops did not pass through all of them, so it is not
// trusted either and the peer address is used.
func clientIP(r *http.Request, trustedHops int) string {
	if trustedHops > 0 {
		var entries []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			for _, e := range strings.Split(h, ",") {
				entries = append(entries, strings.TrimSpace(e))
			}
		}
		if len(entries) >= trustedHops {
			if ip := entries[len(entries)-trustedHops]; ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
