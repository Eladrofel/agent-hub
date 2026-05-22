// Package auth provides Bearer-token middleware for the gateway.
//
// Two trust tiers:
//
//   1. Per-host agent tokens — minted by /v1/admin/agents/:name/mint-token,
//      stored as bcrypt(token) in agents.token_hash. Every /v1/* endpoint
//      (excluding /v1/admin/*) verifies the bearer against active agents,
//      attaches the agent identity to the request context, and updates
//      agents.last_seen_at.
//
//   2. Admin token — a single bearer string supplied via the ADMIN_TOKEN
//      env var. Gates /v1/admin/* (token minting, future operator-only
//      endpoints). Plain constant-time string compare; no DB hit.
//
// Bcrypt-per-request is acceptable at the design's volume (5 peers, ~6500
// events/day busy). Add an in-memory cache in v0.1.x if profiling shows it
// matters.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Agent is the identity attached to authenticated requests.
//
// Alias is the human-friendly display name (sourced from agents.
// mattermost_username, e.g. "Splinter" / "Mikey"). Populated when set;
// empty string when the agent has no alias. Used by handlers that build
// operator-facing summaries (lifecycle events, Mattermost surfaces) so
// the output reads like "Splinter" rather than "agent-operator-mac".
type Agent struct {
	ID          string
	Name        string
	Alias       string
	Role        string
	Permissions map[string]any
}

type ctxKey int

const agentCtxKey ctxKey = 1

// FromContext returns the authenticated agent for an authenticated request.
// Panics if called from a handler that wasn't wrapped in RequireAgent — that's
// a programmer error, surface it loudly.
func FromContext(ctx context.Context) *Agent {
	a, ok := ctx.Value(agentCtxKey).(*Agent)
	if !ok {
		panic("auth: no agent in context — handler is missing RequireAgent middleware")
	}
	return a
}

// Middleware bundles the dependencies the auth layer needs.
type Middleware struct {
	Pool       *pgxpool.Pool
	AdminToken string
}

// RequireAgent verifies the Bearer token against agents.token_hash and
// attaches the matched Agent to the request context. The naive approach
// (scan every agent row, bcrypt-compare each) is fine at fleet scale ≤ ~50;
// revisit if/when the fleet grows.
func (m *Middleware) RequireAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := bearerToken(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "missing_or_malformed_bearer", err.Error())
			return
		}

		agent, err := m.matchAgent(r.Context(), token)
		if err != nil {
			if errors.Is(err, errNoMatch) {
				writeError(w, http.StatusUnauthorized, "invalid_token", "token did not match any registered agent")
				return
			}
			writeError(w, http.StatusInternalServerError, "auth_lookup_failed", err.Error())
			return
		}

		// Best-effort last_seen update; never blocks the request on failure.
		go func(agentID string) {
			_, _ = m.Pool.Exec(context.Background(),
				`UPDATE agents SET last_seen_at = now() WHERE id = $1`, agentID)
		}(agent.ID)

		ctx := context.WithValue(r.Context(), agentCtxKey, agent)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAdmin gates admin endpoints behind ADMIN_TOKEN. No DB lookup; the
// token is operator-managed via .env.
func (m *Middleware) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.AdminToken == "" {
			writeError(w, http.StatusInternalServerError, "admin_token_unset",
				"ADMIN_TOKEN env var is empty; admin endpoints disabled")
			return
		}
		token, err := bearerToken(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "missing_or_malformed_bearer", err.Error())
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(m.AdminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid_admin_token", "admin token mismatch")
			return
		}
		next.ServeHTTP(w, r)
	})
}

var errNoMatch = errors.New("no agent matched")

func (m *Middleware) matchAgent(ctx context.Context, token string) (*Agent, error) {
	rows, err := m.Pool.Query(ctx,
		`SELECT id, name, mattermost_username, role, token_hash, permissions
		   FROM agents
		  WHERE token_hash IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tokenBytes := []byte(token)
	for rows.Next() {
		var (
			id          string
			name        string
			alias       *string
			role        *string
			hash        string
			permsRaw    []byte
			permissions map[string]any
		)
		if err := rows.Scan(&id, &name, &alias, &role, &hash, &permsRaw); err != nil {
			return nil, err
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), tokenBytes) != nil {
			continue
		}
		if len(permsRaw) > 0 {
			_ = json.Unmarshal(permsRaw, &permissions)
		}
		out := &Agent{ID: id, Name: name, Permissions: permissions}
		if alias != nil {
			out.Alias = *alias
		}
		if role != nil {
			out.Role = *role
		}
		return out, nil
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, errNoMatch
}

func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", fmt.Errorf("Authorization header missing")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", fmt.Errorf("Authorization header is not a Bearer credential")
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if tok == "" {
		return "", fmt.Errorf("Bearer token is empty")
	}
	return tok, nil
}

// writeError emits a small JSON envelope. Mirrored from server package style
// to avoid an import cycle; the shape is intentionally minimal.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": msg,
	})
}

// Compile-time assurance pgx is wired correctly.
var _ = pgx.ErrNoRows
