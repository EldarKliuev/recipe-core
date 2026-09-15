// Package api assembles the HTTP surface from the feature packages.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/recipe-ai/core-api/internal/auth"
	"github.com/recipe-ai/core-api/internal/catalog"
	"github.com/recipe-ai/core-api/internal/cook"
	"github.com/recipe-ai/core-api/internal/inventory"
	"github.com/recipe-ai/core-api/internal/platform/config"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	appmw "github.com/recipe-ai/core-api/internal/platform/middleware"
	"github.com/recipe-ai/core-api/internal/recipehistory"
	"github.com/recipe-ai/core-api/internal/user"
)

// Deps are the wired-up handlers and probes the router needs.
type Deps struct {
	Config      config.Config
	Log         *slog.Logger
	Tokens      *auth.TokenIssuer
	RateLimiter *auth.RateLimiter

	Auth      *auth.Handler
	User      *user.Handler
	Inventory *inventory.Handler
	Catalog   *catalog.Handler
	Cook      *cook.Handler
	History   *recipehistory.Handler

	// Ready reports whether required dependencies are reachable. llm-service is
	// deliberately not part of readiness: search must keep serving without it.
	Ready func(ctx context.Context) error
}

func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(chimw.Recoverer)
	r.Use(appmw.RequestLogger(d.Log))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   d.Config.CORSAllowedOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Idempotency-Key"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := d.Ready(ctx); err != nil {
			d.Log.WarnContext(ctx, "readiness check failed", slog.Any("error", err))
			httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"error":  err.Error(),
			})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Mount("/auth", d.Auth.Routes(d.RateLimiter.Middleware))

		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(d.Tokens))

			r.Route("/me", func(r chi.Router) {
				r.Get("/prefs", d.User.GetPrefs)
				r.Put("/prefs", d.User.PutPrefs)
				r.Mount("/ingredients", d.Inventory.Routes())
				r.Mount("/history", d.History.Routes())
			})
			r.Mount("/ingredients", d.Catalog.Routes())
			r.Mount("/cook", d.Cook.Routes())
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusNotFound, map[string]any{
			"error": httpx.Error{Code: "not_found", Message: "no such endpoint"},
		})
	})
	return r
}
