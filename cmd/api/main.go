// Command api is the core-api service: one binary, one deployment, a modular
// monolith over a single Postgres.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/recipe-ai/core-api/internal/api"
	"github.com/recipe-ai/core-api/internal/auth"
	"github.com/recipe-ai/core-api/internal/catalog"
	"github.com/recipe-ai/core-api/internal/cook"
	"github.com/recipe-ai/core-api/internal/inventory"
	"github.com/recipe-ai/core-api/internal/platform/config"
	"github.com/recipe-ai/core-api/internal/platform/logging"
	"github.com/recipe-ai/core-api/internal/platform/postgres"
	"github.com/recipe-ai/core-api/internal/platform/redisclient"
	"github.com/recipe-ai/core-api/internal/recipehistory"
	"github.com/recipe-ai/core-api/internal/user"
)

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisConn, err := redisclient.New(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = redisConn.Close() }()

	// Repositories.
	authRepo := auth.NewRepo(pool)
	userRepo := user.NewRepo(pool)
	inventoryRepo := inventory.NewRepo(pool)
	catalogRepo := catalog.NewRepo(pool)
	retrievalRepo := cook.NewRetrievalRepo(pool)
	historyRepo := recipehistory.NewRepo(pool)

	// Adaptation pipeline. The worker and the cook service reference each other,
	// so the worker is created first and bound afterwards.
	llmClient := cook.NewLLMClient(cfg.LLMServiceURL, cfg.LLMServiceTimeout)
	jobStore := cook.NewJobStore(redisConn, cfg.JobTTL)
	validator := cook.NewValidator(catalogRepo)
	worker := cook.NewWorker(
		cfg.AdaptQueueSize, cfg.AdaptWorkers,
		llmClient, validator, jobStore, historyRepo,
		cfg.LLMServiceTimeout, cfg.MaxAdaptAttempts, log,
	)
	cookSvc := cook.NewService(retrievalRepo, catalogRepo, userRepo, inventoryRepo, jobStore, worker)
	worker.Bind(cookSvc)
	worker.Start(ctx)

	tokens := auth.NewTokenIssuer([]byte(cfg.JWTSecret), cfg.JWTAccessTTL, cfg.JWTRefreshTTL)
	rateLimiter := auth.NewRateLimiter(
		auth.NewRedisCounter(redisConn), cfg.AuthRateLimit, cfg.AuthRateLimitWindow, log)

	handler := api.NewRouter(api.Deps{
		Config:      cfg,
		Log:         log,
		Tokens:      tokens,
		RateLimiter: rateLimiter,
		Auth:        auth.NewHandler(auth.NewService(authRepo, tokens)),
		User:        user.NewHandler(userRepo, catalogRepo),
		Inventory:   inventory.NewHandler(inventoryRepo, catalogRepo),
		Catalog:     catalog.NewHandler(catalogRepo),
		Cook:        cook.NewHandler(cookSvc),
		History:     recipehistory.NewHandler(historyRepo),
		Ready: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("postgres: %w", err)
			}
			if err := redisConn.Ping(ctx).Err(); err != nil {
				return fmt.Errorf("redis: %w", err)
			}
			return nil
		},
	})

	srv := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: handler,
		// No WriteTimeout: the SSE job stream is a long-lived response and a
		// write deadline would sever it mid-generation.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("core-api listening",
			slog.String("addr", srv.Addr),
			slog.String("llm_service_url", cfg.LLMServiceURL))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", slog.Any("error", err))
	}
	worker.Stop()
	log.Info("stopped")
	return nil
}
