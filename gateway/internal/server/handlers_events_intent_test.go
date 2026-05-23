// Integration tests for v0.1.10 payload.intent enforcement.
//
// The gateway accepts intent ∈ {info|directive|question|blocker|status} as
// an OPTIONAL payload field. Two checks apply:
//
//   1. Enum check: unknown values → 400 invalid_intent. Absent / empty is
//      legal and means "info".
//
//   2. Role check: intent=directive requires agents.role='operator'. Anyone
//      else gets 403 directive_not_authorized with the caller's actual role
//      in the response body. Other intents accept from any role.
//
// All checks happen BEFORE sanitiser / resolution / insert so an
// unauthorised directive never writes anything to the DB.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// seedAgentWithRole inserts an agents row with the requested role. Mirrors
// seedAgent (operator default) but is parameterised so tests can mint
// frontend-agent identities. Returns (agent_id, plaintext_token).
func seedAgentWithRole(t *testing.T, st *store.Store, name, role string) (string, string) {
	t.Helper()
	plain := "test-token-" + name
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	var id string
	err = st.Pool.QueryRow(context.Background(),
		`INSERT INTO agents (name, role, token_hash)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		name, role, string(hash)).Scan(&id)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return id, plain
}

// -----------------------------------------------------------------------------
// Happy path: operator can emit any intent
// -----------------------------------------------------------------------------

func TestEventEmit_OperatorCanEmitDirective(t *testing.T) {
	env := newTestEnv(t, "")
	// Default seeded agent is role=operator.

	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "decision.proposed",
		"summary":    "switch the runner to docker",
		"payload":    map[string]any{"intent": "directive"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp eventEmitResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.EventID == "" {
		t.Fatal("event_id empty")
	}
}

// -----------------------------------------------------------------------------
// Enforcement: non-operator emitting directive → 403
// -----------------------------------------------------------------------------

func TestEventEmit_NonOperatorRejectedForDirective(t *testing.T) {
	env := newTestEnv(t, "")
	_, frontendToken := seedAgentWithRole(t, env.store, "agent-frontend", "frontend")

	w := env.request("POST", "/v1/events", frontendToken, map[string]any{
		"event_type": "decision.proposed",
		"summary":    "rewriting auth in rust",
		"payload":    map[string]any{"intent": "directive"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error   string            `json:"error"`
		Message string            `json:"message"`
		Details map[string]string `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != "directive_not_authorized" {
		t.Fatalf("error = %q, want 'directive_not_authorized'", resp.Error)
	}
	if resp.Details["role"] != "frontend" {
		t.Errorf("details.role = %q, want 'frontend'", resp.Details["role"])
	}
	if resp.Details["docs"] == "" {
		t.Error("details.docs should point at the peer-coordination-policy reference")
	}

	// Crucial: the rejected directive must NOT have written an events row.
	var n int
	if err := env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*) FROM events WHERE event_type = 'decision.proposed'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected directive must not write to events; found %d rows", n)
	}
}

// -----------------------------------------------------------------------------
// Non-directive intents accept from any role
// -----------------------------------------------------------------------------

func TestEventEmit_NonOperatorCanEmitInfo(t *testing.T) {
	env := newTestEnv(t, "")
	_, frontendToken := seedAgentWithRole(t, env.store, "agent-frontend", "frontend")

	w := env.request("POST", "/v1/events", frontendToken, map[string]any{
		"event_type": "progress.updated",
		"summary":    "shipped a thing",
		"payload":    map[string]any{"intent": "info"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

func TestEventEmit_NonOperatorCanEmitBlocker(t *testing.T) {
	env := newTestEnv(t, "")
	_, frontendToken := seedAgentWithRole(t, env.store, "agent-frontend", "frontend")

	w := env.request("POST", "/v1/events", frontendToken, map[string]any{
		"event_type": "task.blocked",
		"summary":    "fixture vanished",
		"payload":    map[string]any{"intent": "blocker"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// Absent intent → treated as info (no rejection regardless of role)
// -----------------------------------------------------------------------------

func TestEventEmit_AbsentIntentTreatedAsInfo(t *testing.T) {
	env := newTestEnv(t, "")
	_, frontendToken := seedAgentWithRole(t, env.store, "agent-frontend", "frontend")

	w := env.request("POST", "/v1/events", frontendToken, map[string]any{
		"event_type": "progress.updated",
		"summary":    "no intent set",
		// no payload.intent at all
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// Bad enum value → 400
// -----------------------------------------------------------------------------

func TestEventEmit_InvalidIntentRejected(t *testing.T) {
	env := newTestEnv(t, "")

	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "progress.updated",
		"summary":    "x",
		"payload":    map[string]any{"intent": "urgent"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "invalid_intent" {
		t.Fatalf("error = %q, want 'invalid_intent'", resp.Error)
	}
}

// Non-string payload.intent (caller mis-shaped the field) is currently
// treated as "absent" rather than rejected — the strict enum check still
// guards the documented happy-path; a non-string value just bypasses it
// and the event flows as if no intent was set. This is intentional: the
// enforcement target is operator-vs-peer authority, not payload schema
// hygiene.
func TestEventEmit_NonStringIntentTreatedAsAbsent(t *testing.T) {
	env := newTestEnv(t, "")

	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "progress.updated",
		"summary":    "weird payload",
		"payload":    map[string]any{"intent": 42}, // not a string
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}
