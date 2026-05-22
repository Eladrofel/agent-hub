// Integration tests for the vertical-slice handlers (POST /v1/events + /health).
// These require a live Postgres. Configure via:
//
//	AGENT_HUB_TEST_DATABASE_URL=postgres://agent_hub:<pw>@127.0.0.1:54329/agent_hub?sslmode=disable
//
// Bring one up locally with:
//
//	docker compose up -d postgres
//
// Tests skip if the env var is unset so `go test ./...` stays green on
// machines without a DB.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/agent-hub/gateway/internal/sanitiser"
	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// testEnv is the integration-test fixture. testApp wires the same middleware
// stack Run() uses so the request paths exercised by tests match production.
type testEnv struct {
	t       *testing.T
	ctx     context.Context
	store   *store.Store
	app     *App
	handler http.Handler

	// agentID + plaintext token of a pre-seeded agent. Tests authenticate
	// against the gateway as this agent.
	agentID    string
	agentToken string
}

func newTestEnv(t *testing.T, patternsBody string) *testEnv {
	t.Helper()
	dsn := os.Getenv("AGENT_HUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_HUB_TEST_DATABASE_URL not set; skipping integration tests")
	}

	ctx := context.Background()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	truncateAll(t, st)

	patternsFile := filepath.Join(t.TempDir(), "patterns.txt")
	if err := os.WriteFile(patternsFile, []byte(patternsBody), 0o600); err != nil {
		t.Fatalf("write patterns: %v", err)
	}
	san, err := sanitiser.Load(patternsFile, nil)
	if err != nil {
		t.Fatalf("load patterns: %v", err)
	}

	mw := &auth.Middleware{Pool: st.Pool, AdminToken: "test-admin"}
	app := &App{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:     st,
		Sanitiser: san,
		Auth:      mw,
	}

	agentID, plainToken := seedAgent(t, st, "agent-test")

	r := NewRouter(app, nil)

	return &testEnv{
		t:          t,
		ctx:        ctx,
		store:      st,
		app:        app,
		handler:    r,
		agentID:    agentID,
		agentToken: plainToken,
	}
}

// truncateAll resets every table the integration tests touch. RESTART
// IDENTITY isn't needed (uuid PKs), but CASCADE deals with FK chains.
func truncateAll(t *testing.T, st *store.Store) {
	t.Helper()
	tables := []string{
		"events", "session_checkpoints", "agent_sessions", "handoffs",
		"decisions", "agent_locks", "artifacts", "mattermost_inbox",
		"mattermost_outbox", "tasks", "agents", "projects",
	}
	for _, tbl := range tables {
		_, err := st.Pool.Exec(context.Background(), "TRUNCATE TABLE "+tbl+" CASCADE")
		if err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}

// seedAgent inserts a row in agents with a freshly-bcrypt'd token. Returns
// (agent_id, plaintext_token).
func seedAgent(t *testing.T, st *store.Store, name string) (string, string) {
	t.Helper()
	plain := "test-token-" + name
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	var id string
	err = st.Pool.QueryRow(context.Background(),
		`INSERT INTO agents (name, role, token_hash)
		 VALUES ($1, 'operator', $2)
		 RETURNING id`,
		name, string(hash)).Scan(&id)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return id, plain
}

func (e *testEnv) request(method, path, token string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	r := httptest.NewRequest(method, path, reqBody)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

// =============================================================================
// /health
// =============================================================================

func TestHealth_OK(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("GET", "/health", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("status field = %v, want 'ok'", resp["status"])
	}
}

// =============================================================================
// POST /v1/events
// =============================================================================

func TestEventEmit_HappyPath(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "progress.updated",
		"summary":    "smoke test",
		"payload":    map[string]any{"phase": "spec"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp eventEmitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.EventID == "" {
		t.Fatal("event_id empty")
	}

	// Verify the row landed and carries the auth'd agent id.
	var (
		gotType    string
		gotSummary string
		gotAgentID string
	)
	err := env.store.Pool.QueryRow(env.ctx,
		`SELECT event_type, summary, agent_id FROM events WHERE id = $1`,
		resp.EventID).Scan(&gotType, &gotSummary, &gotAgentID)
	if err != nil {
		t.Fatalf("select event: %v", err)
	}
	if gotType != "progress.updated" {
		t.Fatalf("event_type = %q, want 'progress.updated'", gotType)
	}
	if gotAgentID != env.agentID {
		t.Fatalf("agent_id = %q, want %q (auth'd agent)", gotAgentID, env.agentID)
	}
	if gotSummary != "smoke test" {
		t.Fatalf("summary = %q, want 'smoke test'", gotSummary)
	}
}

func TestEventEmit_RejectsMissingAuth(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/events", "", map[string]any{"event_type": "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestEventEmit_RejectsBadToken(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/events", "wrong-token", map[string]any{"event_type": "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestEventEmit_RejectsMissingEventType(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"summary": "no type",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestEventEmit_SanitiserBlocksAndWritesAuditEvent(t *testing.T) {
	// One pattern: any private 10.x.x.x address.
	env := newTestEnv(t, `\b10\.\d+\.\d+\.\d+\b`+"\n")

	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "progress.updated",
		"summary":    "VM at 10.0.5.50 is down",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}

	var resp sanitiserBlockedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != "sanitiser_blocked" {
		t.Fatalf("error = %q, want 'sanitiser_blocked'", resp.Error)
	}
	if resp.MatchedField != "summary" {
		t.Fatalf("matched_field = %q, want 'summary'", resp.MatchedField)
	}
	if resp.BlockedEventID == "" {
		t.Fatal("blocked_event_id empty; audit event was not written")
	}

	// Verify the audit event landed AND the original event did not.
	var (
		auditType    string
		auditSummary string
		auditPayload []byte
	)
	err := env.store.Pool.QueryRow(env.ctx,
		`SELECT event_type, summary, payload FROM events WHERE id = $1`,
		resp.BlockedEventID).Scan(&auditType, &auditSummary, &auditPayload)
	if err != nil {
		t.Fatalf("select audit event: %v", err)
	}
	if auditType != "sanitiser.blocked" {
		t.Fatalf("audit event_type = %q, want 'sanitiser.blocked'", auditType)
	}
	// The audit summary should NOT contain the original offending content.
	if bytes.Contains([]byte(auditSummary), []byte("10.0.5.50")) {
		t.Fatalf("audit summary leaked original content: %q", auditSummary)
	}
	if bytes.Contains(auditPayload, []byte("10.0.5.50")) {
		t.Fatalf("audit payload leaked original content: %s", auditPayload)
	}

	// Count of events with event_type='progress.updated' should be zero;
	// only the audit row exists.
	var n int
	if err := env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*) FROM events WHERE event_type = 'progress.updated'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("found %d progress.updated rows; want 0 (sanitiser must block)", n)
	}
}

func TestEventEmit_SanitiserBlocksMatchInPayload(t *testing.T) {
	env := newTestEnv(t, "forgejo\n")
	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "decision.proposed",
		"summary":    "harmless summary",
		"payload":    map[string]any{"ref_url": "http://forgejo.example/x"},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	var resp sanitiserBlockedResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.MatchedField != "payload" {
		t.Fatalf("matched_field = %q, want 'payload'", resp.MatchedField)
	}
}

func TestEventEmit_RejectsUnknownTaskKey(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "task.claimed",
		"task_key":   "feat-99-nonexistent",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	var resp errorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "unknown_reference" {
		t.Fatalf("error = %q, want 'unknown_reference'", resp.Error)
	}
}

// =============================================================================
// v0.1.3 Component C — curated event types trigger outbox writes
// =============================================================================

func TestEventEmit_CuratedType_WritesOutboxRow(t *testing.T) {
	env := newTestEnv(t, "")
	env.app.MattermostDefaultOutbox = "agent-events"

	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "task.created",
		"summary":    "feat-04 ready for claim",
		"payload":    map[string]any{"task_key": "feat-04-x"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp eventEmitResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	// Curated → events row AND outbox row exist.
	var (
		evCount     int
		outboxCount int
		outboxCh    string
		outboxMsg   string
	)
	_ = env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*) FROM events WHERE id = $1`, resp.EventID).Scan(&evCount)
	if evCount != 1 {
		t.Fatalf("events row count = %d, want 1", evCount)
	}
	err := env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*), max(channel_id), max(message)
		   FROM mattermost_outbox WHERE event_id = $1`, resp.EventID,
	).Scan(&outboxCount, &outboxCh, &outboxMsg)
	if err != nil {
		t.Fatalf("scan outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox row count = %d, want 1 for curated event", outboxCount)
	}
	if outboxCh != "agent-events" {
		t.Fatalf("outbox channel = %q, want 'agent-events'", outboxCh)
	}
	if outboxMsg == "" {
		t.Fatal("outbox message empty")
	}
}

func TestEventEmit_NonCuratedType_NoOutboxRow(t *testing.T) {
	env := newTestEnv(t, "")
	env.app.MattermostDefaultOutbox = "agent-events"

	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "progress.updated", // NOT in CuratedEventTypes
		"summary":    "noisy progress ping",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	var resp eventEmitResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	var outboxCount int
	_ = env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*) FROM mattermost_outbox WHERE event_id = $1`, resp.EventID,
	).Scan(&outboxCount)
	if outboxCount != 0 {
		t.Fatalf("outbox row count = %d, want 0 for non-curated event", outboxCount)
	}
}

func TestEventEmit_CuratedType_NoDefaultChannelSkipsOutbox(t *testing.T) {
	env := newTestEnv(t, "")
	env.app.MattermostDefaultOutbox = "" // operator hasn't configured default

	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "task.completed",
		"summary":    "no channel configured",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	var resp eventEmitResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	var outboxCount int
	_ = env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*) FROM mattermost_outbox WHERE event_id = $1`, resp.EventID,
	).Scan(&outboxCount)
	if outboxCount != 0 {
		t.Fatalf("outbox row count = %d, want 0 when no channel resolves", outboxCount)
	}
}

// =============================================================================
// v0.1.9 — agent.improvement-note is curated + uses the dedicated formatter
// =============================================================================

func TestEventEmit_ImprovementNote_FormattedMessageHitsOutbox(t *testing.T) {
	env := newTestEnv(t, "")
	env.app.MattermostDefaultOutbox = "agent-events"

	// Give the seeded agent an alias so the formatter shows "Splinter:" not
	// the canonical agent name. Loaded into auth context via the existing
	// agents.mattermost_username column.
	_, err := env.store.Pool.Exec(env.ctx,
		`UPDATE agents SET mattermost_username = 'Splinter' WHERE id = $1`, env.agentID)
	if err != nil {
		t.Fatalf("set alias: %v", err)
	}

	w := env.request("POST", "/v1/events", env.agentToken, map[string]any{
		"event_type": "agent.improvement-note",
		"summary":    "bot-to-bot relay needs config toggle",
		"payload": map[string]any{
			"category":         "process",
			"summary":          "bot-to-bot relay needs config toggle",
			"propagation_hint": "mm",
			"context":          "v0.1.7 smoke",
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp eventEmitResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	var (
		outboxCount int
		outboxMsg   string
	)
	err = env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*), max(message) FROM mattermost_outbox WHERE event_id = $1`,
		resp.EventID).Scan(&outboxCount, &outboxMsg)
	if err != nil {
		t.Fatalf("scan outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("improvement-note should produce 1 outbox row; got %d", outboxCount)
	}
	want := "\xf0\x9f\x92\xa1 Splinter: bot-to-bot relay needs config toggle _(v0.1.7 smoke)_"
	if outboxMsg != want {
		t.Fatalf("outbox message\n got  %q\n want %q", outboxMsg, want)
	}
}

// silence unused-import warning when test-only constants drop out
var _ = fmt.Sprintf
