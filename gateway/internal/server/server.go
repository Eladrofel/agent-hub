// Package server is the HTTP gateway entrypoint. It composes the store, auth
// middleware, sanitiser, and per-domain handlers into a chi router and runs
// it. The `agent-hub serve` subcommand calls Run.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/agent-hub/gateway/internal/sanitiser"
	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// Config is the runtime configuration for the gateway. Fields map 1:1 to
// env vars consumed in cmd/agent-hub/main.go.
type Config struct {
	ListenAddr              string   // LISTEN_ADDR, default :8787
	DatabaseURL             string   // DATABASE_URL
	AdminToken              string   // ADMIN_TOKEN
	SanitiserPatternsFile   string   // SANITISER_PATTERNS_FILE
	SanitiserExemptHosts    []string // SANITISER_EXEMPT_HOSTS (comma-split)
	MattermostDefaultOutbox string   // MATTERMOST_DEFAULT_OUTBOX_CHANNEL
}

// Run boots the gateway and blocks until ctx is cancelled. Migration is
// applied before the listener starts so a fresh cluster comes up green.
func Run(ctx context.Context, cfg Config) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	logger.Info("migrations applied")

	san, err := sanitiser.Load(cfg.SanitiserPatternsFile, cfg.SanitiserExemptHosts)
	if err != nil {
		return fmt.Errorf("load sanitiser patterns: %w", err)
	}
	logger.Info("sanitiser loaded",
		"patterns", san.Count(),
		"exempt_hosts", len(cfg.SanitiserExemptHosts),
		"file", cfg.SanitiserPatternsFile)

	mw := &auth.Middleware{Pool: st.Pool, AdminToken: cfg.AdminToken}

	app := &App{
		Logger:                 logger,
		Store:                  st,
		Sanitiser:              san,
		Auth:                   mw,
		MattermostDefaultOutbox: cfg.MattermostDefaultOutbox,
	}

	r := NewRouter(app, loggingMiddleware(logger))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// App carries the long-lived dependencies handlers need. Splitting this from
// the chi-router wiring keeps handlers testable in isolation.
type App struct {
	Logger                  *slog.Logger
	Store                   *store.Store
	Sanitiser               *sanitiser.Sanitiser
	Auth                    *auth.Middleware
	MattermostDefaultOutbox string // fallback channel for curated events
}

func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
			)
		})
	}
}
