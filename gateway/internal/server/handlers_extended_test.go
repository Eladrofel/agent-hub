package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Eladrofel/agent-hub/gateway/internal/agents"
	"github.com/Eladrofel/agent-hub/gateway/internal/inbox"
	"github.com/Eladrofel/agent-hub/gateway/internal/sessions"
)

// =============================================================================
// POST /v1/agents/register
// =============================================================================

func TestAgentRegister_HappyPath(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/agents/register", env.agentToken, map[string]any{
		"name":        "agent-test",
		"role":        "operator",
		"host_kind":   "macos",
		"vm_hostname": "test-host",
		"capabilities": []any{"backend", "review"},
		"metadata":    map[string]any{"sdk": "go"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got agents.Agent
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "agent-test" {
		t.Fatalf("name = %q", got.Name)
	}
	if got.HostKind == nil || *got.HostKind != "macos" {
		t.Fatalf("host_kind = %v, want 'macos'", got.HostKind)
	}
	if len(got.Capabilities) != 2 {
		t.Fatalf("capabilities len = %d, want 2", len(got.Capabilities))
	}
}

func TestAgentRegister_RejectsNameMismatch(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/agents/register", env.agentToken, map[string]any{
		"name": "agent-other",
		"role": "operator",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// =============================================================================
// POST /v1/sessions/start + /checkpoint + /end
// =============================================================================

func TestSessionLifecycle(t *testing.T) {
	env := newTestEnv(t, "")

	const cid = "session-lifecycle-1"

	// start
	w := env.request("POST", "/v1/sessions/start", env.agentToken, map[string]any{
		"claude_session_id": cid,
		"branch":            "main",
		"cwd":               "/tmp",
		"start_reason":      "test",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body=%s", w.Code, w.Body.String())
	}
	var sess sessions.Session
	if err := json.Unmarshal(w.Body.Bytes(), &sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if sess.Status != "active" {
		t.Fatalf("status = %q, want active", sess.Status)
	}

	// session.started event landed
	assertEventOfType(t, env, cid, "session.started", 1)

	// checkpoint
	w = env.request("POST", "/v1/sessions/checkpoint", env.agentToken, map[string]any{
		"claude_session_id": cid,
		"summary":           "first checkpoint",
		"current_goal":      "validate flow",
		"next_actions":      []any{"call /sessions/end"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("checkpoint status = %d, body=%s", w.Code, w.Body.String())
	}
	var ckpt sessions.Checkpoint
	if err := json.Unmarshal(w.Body.Bytes(), &ckpt); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	if ckpt.Summary != "first checkpoint" {
		t.Fatalf("summary = %q", ckpt.Summary)
	}
	assertEventOfType(t, env, cid, "session.checkpointed", 1)

	// end
	w = env.request("POST", "/v1/sessions/end", env.agentToken, map[string]any{
		"claude_session_id": cid,
		"final_status":      "task_completed",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("end status = %d, body=%s", w.Code, w.Body.String())
	}
	var ended sessions.Session
	_ = json.Unmarshal(w.Body.Bytes(), &ended)
	if ended.Status != "ended" {
		t.Fatalf("status after end = %q, want ended", ended.Status)
	}
	if ended.EndedAt == nil {
		t.Fatal("ended_at not set")
	}
	assertEventOfType(t, env, cid, "session.ended", 1)
}

func TestSessionCheckpoint_RejectsUnknownSession(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/sessions/checkpoint", env.agentToken, map[string]any{
		"claude_session_id": "does-not-exist",
		"summary":           "ignored",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

// =============================================================================
// GET /v1/sessions/:id/resume-context
// =============================================================================

func TestResumeContext_HappyPath(t *testing.T) {
	env := newTestEnv(t, "")

	const cid = "session-resume-1"
	_ = env.request("POST", "/v1/sessions/start", env.agentToken, map[string]any{
		"claude_session_id": cid,
		"branch":            "feature-x",
	})
	_ = env.request("POST", "/v1/sessions/checkpoint", env.agentToken, map[string]any{
		"claude_session_id": cid,
		"summary":           "halfway done",
		"current_goal":      "ship v0.1.0",
	})
	_ = env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type":        "progress.updated",
		"claude_session_id": cid,
		"summary":           "ran the tests",
	})

	w := env.request("GET", "/v1/sessions/"+cid+"/resume-context", env.agentToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var packet sessions.ResumePacket
	if err := json.Unmarshal(w.Body.Bytes(), &packet); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if packet.Session == nil || packet.Session.ClaudeSessionID != cid {
		t.Fatalf("session = %+v", packet.Session)
	}
	if packet.Checkpoint == nil || packet.Checkpoint.Summary != "halfway done" {
		t.Fatalf("checkpoint = %+v", packet.Checkpoint)
	}
	// Recent events should include session.started, session.checkpointed,
	// progress.updated — at least 3.
	if len(packet.RecentEvents) < 3 {
		t.Fatalf("recent_events len = %d, want >= 3", len(packet.RecentEvents))
	}
}

// Calls /v1/sessions/.../resume-context twice and confirms the response body
// is byte-identical. This is the V2 critical AC ("brief identical before and
// after /clear"). /clear doesn't actually happen here — but since both calls
// read from the same Postgres state, byte equality is the right gate.
func TestResumeContext_IdempotentReads(t *testing.T) {
	env := newTestEnv(t, "")
	const cid = "session-idem-1"
	_ = env.request("POST", "/v1/sessions/start", env.agentToken, map[string]any{
		"claude_session_id": cid,
	})
	_ = env.request("POST", "/v1/sessions/checkpoint", env.agentToken, map[string]any{
		"claude_session_id": cid, "summary": "x",
	})

	w1 := env.request("GET", "/v1/sessions/"+cid+"/resume-context", env.agentToken, nil)
	w2 := env.request("GET", "/v1/sessions/"+cid+"/resume-context", env.agentToken, nil)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("statuses = %d / %d", w1.Code, w2.Code)
	}
	if w1.Body.String() != w2.Body.String() {
		t.Fatalf("resume-context not identical across reads:\n  first=%s\n  second=%s",
			w1.Body.String(), w2.Body.String())
	}
}

func TestResumeContext_RejectsCrossAgentRead(t *testing.T) {
	env := newTestEnv(t, "")

	// Seed a second agent and a session owned by them.
	otherID, _ := seedAgent(t, env.store, "agent-other")
	_, err := env.store.Pool.Exec(env.ctx,
		`INSERT INTO agent_sessions (claude_session_id, agent_id) VALUES ($1, $2)`,
		"other-session", otherID)
	if err != nil {
		t.Fatal(err)
	}

	// agent-test (caller) is role=operator in the fixture seed. Switch to
	// non-operator role so the cross-agent guard actually fires.
	_, err = env.store.Pool.Exec(env.ctx,
		`UPDATE agents SET role = 'backend' WHERE name = 'agent-test'`)
	if err != nil {
		t.Fatal(err)
	}

	w := env.request("GET", "/v1/sessions/other-session/resume-context", env.agentToken, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// =============================================================================
// GET /v1/inbox
// =============================================================================

func TestInboxPoll_EmptyByDefault(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("GET", "/v1/inbox?agent_name=agent-test", env.agentToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		AgentName string          `json:"agent_name"`
		Messages  []inbox.Message `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AgentName != "agent-test" {
		t.Fatalf("agent_name = %q", resp.AgentName)
	}
	if len(resp.Messages) != 0 {
		t.Fatalf("messages len = %d, want 0 (no inbox-webhook writer pre-v0.1.1)", len(resp.Messages))
	}
}

func TestInboxPoll_ReturnsAndMarksDelivered(t *testing.T) {
	env := newTestEnv(t, "")

	// Seed a project (required for FK on mattermost_inbox.project_id; nullable
	// but we want to test the happy path with a real project).
	var projectID string
	err := env.store.Pool.QueryRow(env.ctx,
		`INSERT INTO projects (slug, name) VALUES ($1, $2) RETURNING id`,
		"test-project", "Test").Scan(&projectID)
	if err != nil {
		t.Fatal(err)
	}

	// Manually insert two inbox rows for the caller agent — pre-Component C
	// the inbox-webhook doesn't exist, so direct insert is the only way to
	// test the poll-and-mark-delivered flow.
	for i, body := range []string{"hello agent-test", "second message"} {
		_, err := env.store.Pool.Exec(env.ctx,
			`INSERT INTO mattermost_inbox (project_id, target_agent_id, source_username, message)
			 VALUES ($1, $2, $3, $4)`,
			projectID, env.agentID, "operator", body)
		if err != nil {
			t.Fatalf("seed inbox row %d: %v", i, err)
		}
	}

	w := env.request("GET", "/v1/inbox?agent_name=agent-test", env.agentToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Messages []inbox.Message `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Messages) != 2 {
		t.Fatalf("first poll messages len = %d, want 2", len(resp.Messages))
	}

	// Second poll: rows should be marked delivered, returning empty.
	w2 := env.request("GET", "/v1/inbox?agent_name=agent-test", env.agentToken, nil)
	var resp2 struct {
		Messages []inbox.Message `json:"messages"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if len(resp2.Messages) != 0 {
		t.Fatalf("second poll messages len = %d, want 0 (rows should be marked delivered)", len(resp2.Messages))
	}
}

func TestInboxPoll_RejectsCrossAgentByDefault(t *testing.T) {
	env := newTestEnv(t, "")
	_, _ = seedAgent(t, env.store, "agent-other")

	// Fixture seeds agent-test as operator-role; flip to non-operator to test guard.
	_, err := env.store.Pool.Exec(env.ctx,
		`UPDATE agents SET role = 'backend' WHERE name = 'agent-test'`)
	if err != nil {
		t.Fatal(err)
	}

	w := env.request("GET", "/v1/inbox?agent_name=agent-other", env.agentToken, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// =============================================================================
// POST /v1/admin/agents/{name}/mint-token
// =============================================================================

func TestMintToken_HappyPath(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/admin/agents/agent-new/mint-token", "test-admin", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp mintTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "agent-new" {
		t.Fatalf("name = %q", resp.Name)
	}
	if len(resp.Token) < 32 {
		t.Fatalf("token len = %d, want >= 32", len(resp.Token))
	}

	// Use the minted token to register the new agent — proves the bcrypt
	// stamp matches what auth.RequireAgent expects.
	w2 := env.request("POST", "/v1/agents/register", resp.Token, map[string]any{
		"name": "agent-new",
		"role": "backend",
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("register with minted token failed: status=%d body=%s",
			w2.Code, w2.Body.String())
	}
}

func TestMintToken_RejectsBadAdmin(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/admin/agents/agent-x/mint-token", "wrong-admin", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMintToken_RejectsAgentToken(t *testing.T) {
	env := newTestEnv(t, "")
	// Per-host agent tokens cannot reach the admin namespace.
	w := env.request("POST", "/v1/admin/agents/agent-x/mint-token", env.agentToken, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMintToken_RotatesExistingAgent(t *testing.T) {
	env := newTestEnv(t, "")
	// agent-test is already seeded; mint a new token and confirm the old one
	// stops working.
	oldToken := env.agentToken
	w := env.request("POST", "/v1/admin/agents/agent-test/mint-token", "test-admin", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint status = %d", w.Code)
	}
	var resp mintTokenResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	// Old token rejected.
	w1 := env.request("POST", "/v1/events", oldToken, map[string]any{
		"event_type": "x",
	})
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("old token still works: status=%d", w1.Code)
	}
	// New token accepted.
	w2 := env.request("POST", "/v1/events", resp.Token, map[string]any{
		"event_type": "ok",
	})
	if w2.Code != http.StatusCreated {
		t.Fatalf("new token rejected: status=%d body=%s", w2.Code, w2.Body.String())
	}
}

// =============================================================================
// helpers
// =============================================================================

func assertEventOfType(t *testing.T, env *testEnv, claudeSessionID, eventType string, want int) {
	t.Helper()
	var n int
	err := env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*) FROM events
		 WHERE claude_session_id = $1 AND event_type = $2`,
		claudeSessionID, eventType).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("count(event_type=%s, cid=%s) = %d, want %d",
			eventType, claudeSessionID, n, want)
	}
}

// silence unused-import warning when the test file shrinks
var _ = fmt.Sprintf
