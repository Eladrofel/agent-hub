package server

import (
	"context"
	"net/http"
	"time"
)

// handleHealth is the no-auth liveness probe. Returns 200 with a small JSON
// body iff the gateway can reach Postgres. The Docker healthcheck and the
// plugin's /agent-events-health both call this; keep the response shape
// stable across releases.
func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := a.Store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "postgres_unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "ok",
		"sanitiser_patterns": a.Sanitiser.Count(),
	})
}
