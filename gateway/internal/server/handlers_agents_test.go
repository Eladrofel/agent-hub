// Integration tests for GET /v1/agents/{name_or_alias}/latest-session
// (v0.1.12). Backs the `agentctl resume-context` no-flag fallback. DB-gated
// via AGENT_HUB_TEST_DATABASE_URL — skip when unset.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// seedSession inserts an agent_sessions row with an explicit started_at so
// "latest" assertions are deterministic. Returns the agent_session_id.
func seedSession(t *testing.T, env *testEnv, agentID, claudeSessionID string, startedAt time.Time) string {
	t.Helper()
	var id string
	err := env.store.Pool.QueryRow(env.ctx,
		`INSERT INTO agent_sessions (claude_session_id, agent_id, started_at, status, metadata)
		 VALUES ($1, $2, $3, 'active', '{}'::jsonb)
		 RETURNING id`,
		claudeSessionID, agentID, startedAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

func TestAgentLatestSession_ReturnsMostRecent(t *testing.T) {
	env := newTestEnv(t, "")

	// Two sessions for the seeded agent-test. The second is more recent.
	older := time.Now().Add(-2 * time.Hour).UTC()
	newer := time.Now().Add(-30 * time.Minute).UTC()
	seedSession(t, env, env.agentID, "sess-old-aaa", older)
	seedSession(t, env, env.agentID, "sess-new-bbb", newer)

	w := env.request("GET", "/v1/agents/agent-test/latest-session", "test-admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp latestSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AgentName != "agent-test" {
		t.Fatalf("agent_name = %q", resp.AgentName)
	}
	if resp.LatestSession == nil {
		t.Fatal("latest_session is nil")
	}
	if resp.LatestSession.ClaudeSessionID != "sess-new-bbb" {
		t.Fatalf("latest claude_session_id = %q, want sess-new-bbb", resp.LatestSession.ClaudeSessionID)
	}
	if resp.LatestSession.Status != "active" {
		t.Fatalf("status = %q", resp.LatestSession.Status)
	}
}

func TestAgentLatestSession_ExcludeFiltersCurrent(t *testing.T) {
	env := newTestEnv(t, "")

	older := time.Now().Add(-2 * time.Hour).UTC()
	newer := time.Now().Add(-30 * time.Minute).UTC()
	seedSession(t, env, env.agentID, "sess-prior", older)
	seedSession(t, env, env.agentID, "sess-current-new", newer)

	// Without exclude: returns the newest (current).
	w1 := env.request("GET", "/v1/agents/agent-test/latest-session", "test-admin", nil)
	if w1.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w1.Code, w1.Body.String())
	}
	var r1 latestSessionResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)
	if r1.LatestSession.ClaudeSessionID != "sess-current-new" {
		t.Fatalf("baseline: got %q, want sess-current-new", r1.LatestSession.ClaudeSessionID)
	}

	// With exclude=sess-current-new: returns the prior. This is the
	// post-/clear case — operator is in the new shell, wants the old session.
	w2 := env.request("GET", "/v1/agents/agent-test/latest-session?exclude=sess-current-new", "test-admin", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w2.Code, w2.Body.String())
	}
	var r2 latestSessionResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)
	if r2.LatestSession.ClaudeSessionID != "sess-prior" {
		t.Fatalf("excluded: got %q, want sess-prior", r2.LatestSession.ClaudeSessionID)
	}
}

func TestAgentLatestSession_UnknownAgent_404(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("GET", "/v1/agents/agent-does-not-exist/latest-session", "test-admin", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "unknown_agent" {
		t.Fatalf("error = %v, want unknown_agent", resp["error"])
	}
}

func TestAgentLatestSession_NoSessionsForAgent_404(t *testing.T) {
	env := newTestEnv(t, "")
	// Agent exists (seeded by newTestEnv) but has no agent_sessions rows.
	w := env.request("GET", "/v1/agents/agent-test/latest-session", "test-admin", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "no_sessions" {
		t.Fatalf("error = %v, want no_sessions", resp["error"])
	}
}

func TestAgentLatestSession_CaseInsensitiveByAlias(t *testing.T) {
	env := newTestEnv(t, "")
	// Set an alias on the seeded agent.
	if _, err := env.store.Pool.Exec(context.Background(),
		`UPDATE agents SET mattermost_username = 'Splinter' WHERE id = $1`, env.agentID); err != nil {
		t.Fatalf("set alias: %v", err)
	}
	seedSession(t, env, env.agentID, "sess-alias-case", time.Now().UTC())

	// "splinter" (lowercase) must resolve to "Splinter" — matches the v0.1.8
	// #45 case-insensitive lookup contract.
	w := env.request("GET", "/v1/agents/splinter/latest-session", "test-admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var r latestSessionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if r.LatestSession == nil || r.LatestSession.ClaudeSessionID != "sess-alias-case" {
		t.Fatalf("alias lookup failed: %+v", r)
	}
	if r.Alias == nil || *r.Alias != "Splinter" {
		t.Fatalf("alias field = %v, want 'Splinter'", r.Alias)
	}
}

func TestAgentLatestSession_RejectsMissingAdminAuth(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("GET", "/v1/agents/agent-test/latest-session", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
