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

	// Public /dist/* binary serving — outside any auth middleware. Fresh
	// VMs `curl` agentctl from here during /join before they have any
	// credentials. 404 if file missing; 503 if DistDir unset.
	//
	// HEAD is registered alongside GET so caching proxies + link-checkers
	// that probe before downloading don't see 405 (chi doesn't auto-derive
	// HEAD from GET). The handler uses http.ServeContent which strips the
	// body on HEAD automatically.
	r.Get("/dist/agentctl-linux-amd64", app.handleDistAgentctl("agentctl-linux-amd64"))
	r.Head("/dist/agentctl-linux-amd64", app.handleDistAgentctl("agentctl-linux-amd64"))
	r.Get("/dist/agentctl-darwin-arm64", app.handleDistAgentctl("agentctl-darwin-arm64"))
	r.Head("/dist/agentctl-darwin-arm64", app.handleDistAgentctl("agentctl-darwin-arm64"))

	// Public join-code redemption — the signed code IS the authentication.
	r.Post("/v1/join-codes/redeem", app.handleJoinCodeRedeem)

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
			// v0.1.13 — self-scoped latest-session lookup for the /resume-context skill.
			// Per-host bearer only (no admin required); operates on caller.ID.
			r.Get("/me/latest-session", app.handleMeLatestSession)
			r.Get("/inbox", app.handleInboxPoll)
		})
		// Admin endpoints (ADMIN_TOKEN env var).
		r.Group(func(r chi.Router) {
			r.Use(app.Auth.RequireAdmin)
			r.Post("/admin/agents/{name}/mint-token", app.handleMintToken)

			// Operator-only query endpoints (issue #43).
			r.Get("/agents", app.handleAgentsList)
			// v0.1.12 — backs the `agentctl resume-context` no-flag fallback.
			r.Get("/agents/{name_or_alias}/latest-session", app.handleAgentLatestSession)
			r.Get("/events", app.handleEventsList)
			r.Get("/health/full", app.handleHealthFull)
		})
		// Admin endpoints with additional X-Mint-Authority dual-auth.
		// Track C contract: RequireAdmin returns 401 for missing/wrong
		// Authorization; requireMintAuthority returns 403 for missing/
		// wrong X-Mint-Authority.
		r.Group(func(r chi.Router) {
			r.Use(app.Auth.RequireAdmin)
			r.Use(app.requireMintAuthority)
			r.Post("/admin/join-codes", app.handleJoinCodeMint)
		})
	})

	return r
}
