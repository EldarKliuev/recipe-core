// Package config loads service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPPort           string
	LogLevel           string
	DatabaseURL        string
	RedisURL           string
	JWTSecret          string
	JWTAccessTTL       time.Duration
	JWTRefreshTTL      time.Duration
	LLMServiceURL      string
	LLMServiceTimeout  time.Duration
	CORSAllowedOrigins []string

	// AdaptWorkers is the size of the in-process job worker pool.
	AdaptWorkers int
	// AdaptQueueSize bounds the buffered job channel.
	AdaptQueueSize int
	// JobTTL is how long job status and results live in Redis.
	JobTTL time.Duration
	// MaxAdaptAttempts is how many times a model answer may be rejected by the
	// validator before we fall back to the unadapted recipe.
	MaxAdaptAttempts int
	// AuthRateLimit is the number of auth attempts allowed per window per IP.
	AuthRateLimit       int
	AuthRateLimitWindow time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		HTTPPort:            env("HTTP_PORT", "8080"),
		LogLevel:            env("LOG_LEVEL", "info"),
		DatabaseURL:         env("DATABASE_URL", ""),
		RedisURL:            env("REDIS_URL", ""),
		JWTSecret:           env("JWT_SECRET", ""),
		LLMServiceURL:       env("LLM_SERVICE_URL", "http://llm-service:9000"),
		CORSAllowedOrigins:  splitList(env("CORS_ALLOWED_ORIGINS", "http://localhost:5173")),
		MaxAdaptAttempts:    2,
		AuthRateLimit:       10,
		AuthRateLimitWindow: time.Minute,
	}

	var err error
	if cfg.JWTAccessTTL, err = envDuration("JWT_ACCESS_TTL", 15*time.Minute); err != nil {
		return cfg, err
	}
	if cfg.JWTRefreshTTL, err = envDuration("JWT_REFRESH_TTL", 30*24*time.Hour); err != nil {
		return cfg, err
	}
	if cfg.JobTTL, err = envDuration("JOB_TTL", time.Hour); err != nil {
		return cfg, err
	}
	timeoutSec, err := envInt("LLM_SERVICE_TIMEOUT_SEC", 180)
	if err != nil {
		return cfg, err
	}
	cfg.LLMServiceTimeout = time.Duration(timeoutSec) * time.Second

	if cfg.AdaptWorkers, err = envInt("ADAPT_WORKERS", 2); err != nil {
		return cfg, err
	}
	if cfg.AdaptQueueSize, err = envInt("ADAPT_QUEUE_SIZE", 64); err != nil {
		return cfg, err
	}

	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.RedisURL == "" {
		return cfg, fmt.Errorf("REDIS_URL is required")
	}
	if len(cfg.JWTSecret) < 32 {
		return cfg, fmt.Errorf("JWT_SECRET is required and must be at least 32 bytes")
	}
	return cfg, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

// envDuration accepts either a Go duration ("15m") or a bare number of seconds.
func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
