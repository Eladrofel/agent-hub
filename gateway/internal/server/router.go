package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter wires every route into a chi.Router. Extracted so tests can mount
// the exact same route table the production binary serves — drift between
// "what tests cover" and "what serves traffic" is the bug class this guards
// against.
//
// extraMiddleware lets the production caller add request-logging without
// forcing tests to pull in a log sink. Pass nil from tests.
func NewRouter(app *App, extraMiddleware func(http.Handler) http.Handler) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	if extraMiddleware != nil {
		r.Use(extraMiddleware)
	}

	r.Get("/health", app.handleHealth)
	r.Head("/health", app.handleHealth) // `wget --spider` (docker healthcheck) sends HEAD

	r.Route("/v1", func(r chi.Router) {
		// Per-host token endpoints.
		r.Group(func(r chi.Router) {
			r.Use(app.Auth.RequireAgent)
			r.Post("/events", app.handleEventEmit)
			r.Post("/agents/register", app.handleAgentRegister)
			r.Post("/projects", app.handleProjectUpsert)
			r.Get("/projects", app.handleProjectList)
			r.Get("/projects/{slug}", app.handleProjectGet)
			r.Post("/sessions/start", app.handleSessionStart)
			r.Post("/sessions/checkpoint", app.handleSessionCheckpoint)
			r.Post("/sessions/end", app.handleSessionEnd)
			r.Get("/sessions/{claude_session_id}/resume-context", app.handleSessionResumeContext)
			r.Get("/inbox", app.handleInboxPoll)
		})
		// Admin endpoints (ADMIN_TOKEN env var).
		r.Group(func(r chi.Router) {
			r.Use(app.Auth.RequireAdmin)
			r.Post("/admin/agents/{name}/mint-token", app.handleMintToken)

			// Operator-only query endpoints (issue #43).
			r.Get("/agents", app.handleAgentsList)
			r.Get("/events", app.handleEventsList)
			r.Get("/health/full", app.handleHealthFull)
		})
	})

	return r
}
