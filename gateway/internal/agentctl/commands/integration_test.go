// Integration tests for agentctl subcommands. These wire the actual gateway
// chi router (via server.NewRouter) against a live Postgres and exercise
// each subcommand end-to-end. Skip if AGENT_HUB_TEST_DATABASE_URL is unset,
// matching the gateway's own test convention.
package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/agent-hub/gateway/internal/sanitiser"
	"github.com/Eladrofel/agent-hub/gateway/internal/server"
	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// intEnv mirrors server_test.testEnv but exposes the httptest server URL so
// agentctl's HTTP client can hit it the same way it would hit production.
type intEnv struct {
	t          *testing.T
	httpURL    string
	tokenPath  string
	auditPath  string
	store      *store.Store
	agentName  string
}

func newIntEnv(t *testing.T) *intEnv {
	t.Helper()
	dsn := os.Getenv("AGENT_HUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_HUB_TEST_DATABASE_URL not set; skipping agentctl integration tests")
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

	// Truncate every table our subcommands might touch.
	for _, tbl := range []string{
		"events", "session_checkpoints", "agent_sessions", "handoffs",
		"decisions", "agent_locks", "artifacts", "mattermost_inbox",
		"mattermost_outbox", "tasks", "agents", "projects",
	} {
		if _, err := st.Pool.Exec(ctx, "TRUNCATE TABLE "+tbl+" CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}

	// Seed one agent with a known token.
	agentName := "agent-int-test"
	plainToken := "int-token-" + agentName
	hash, err := bcrypt.GenerateFromPassword([]byte(plainToken), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	var agentID string
	err = st.Pool.QueryRow(ctx,
		`INSERT INTO agents (name, role, token_hash)
		 VALUES ($1, 'operator', $2) RETURNING id`,
		agentName, string(hash)).Scan(&agentID)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Seed a project so events with project_slug='int-project' resolve.
	_, err = st.Pool.Exec(ctx,
		`INSERT INTO projects (slug, name) VALUES ('int-project', 'int') ON CONFLICT (slug) DO NOTHING`)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	patternsFile := filepath.Join(t.TempDir(), "patterns.txt")
	if err := os.WriteFile(patternsFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	san, err := sanitiser.Load(patternsFile)
	if err != nil {
		t.Fatal(err)
	}

	mw := &auth.Middleware{Pool: st.Pool, AdminToken: "int-admin"}
	app := &server.App{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:     st,
		Sanitiser: san,
		Auth:      mw,
	}
	router := server.NewRouter(app, nil)

	httpSrv := httptest.NewServer(router)
	t.Cleanup(httpSrv.Close)

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenPath, []byte(plainToken), 0o600); err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(dir, "audit.log")

	t.Setenv(config.EnvURL, httpSrv.URL)
	t.Setenv(config.EnvTokenFile, tokenPath)
	t.Setenv(config.EnvAgentName, agentName)
	t.Setenv(config.EnvProjectSlug, "int-project")
	t.Setenv(config.EnvAuditLog, auditPath)

	return &intEnv{
		t:         t,
		httpURL:   httpSrv.URL,
		tokenPath: tokenPath,
		auditPath: auditPath,
		store:     st,
		agentName: agentName,
	}
}

func TestIntegration_Health(t *testing.T) {
	env := newIntEnv(t)
	_ = env
	f := newCapFixture(t)
	if err := f.run(NewHealthCmd()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestIntegration_RegisterAgent(t *testing.T) {
	env := newIntEnv(t)
	f := newCapFixture(t)
	err := f.run(NewRegisterAgentCmd(), "--role", "operator")
	if err != nil {
		t.Fatalf("register: %v stderr=%s", err, f.stderr.String())
	}
	// Verify the row got updated.
	var role string
	err = env.store.Pool.QueryRow(context.Background(),
		`SELECT role FROM agents WHERE name = $1`, env.agentName).Scan(&role)
	if err != nil || role != "operator" {
		t.Fatalf("role=%q err=%v", role, err)
	}
}

func TestIntegration_SessionStartCheckpointEnd(t *testing.T) {
	env := newIntEnv(t)
	claudeID := "claude-int-test-1"

	// start
	f1 := newCapFixture(t)
	if err := f1.run(NewSessionStartCmd(), "--claude-session-id", claudeID); err != nil {
		t.Fatalf("start: %v stderr=%s", err, f1.stderr.String())
	}

	// checkpoint
	f2 := newCapFixture(t)
	if err := f2.run(NewCheckpointCmd(),
		"--claude-session-id", claudeID,
		"--summary", "halfway",
		"--next", "next thing",
	); err != nil {
		t.Fatalf("checkpoint: %v stderr=%s", err, f2.stderr.String())
	}

	// end
	f3 := newCapFixture(t)
	if err := f3.run(NewSessionEndCmd(),
		"--claude-session-id", claudeID,
		"--final-status", "task_completed",
	); err != nil {
		t.Fatalf("end: %v stderr=%s", err, f3.stderr.String())
	}

	// Verify the session row landed.
	var status string
	err := env.store.Pool.QueryRow(context.Background(),
		`SELECT status FROM agent_sessions WHERE claude_session_id = $1`, claudeID).Scan(&status)
	if err != nil {
		t.Fatalf("select session: %v", err)
	}
	if status != "ended" {
		t.Fatalf("session status = %q, want ended", status)
	}
}

func TestIntegration_EventEmit(t *testing.T) {
	env := newIntEnv(t)
	f := newCapFixture(t)
	err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--summary", "int-test",
	)
	if err != nil {
		t.Fatalf("event emit: %v stderr=%s", err, f.stderr.String())
	}
	// Verify one row landed.
	var n int
	if err := env.store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE event_type = 'progress.updated'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d events, want 1", n)
	}
}

func TestIntegration_ResumeContext(t *testing.T) {
	env := newIntEnv(t)
	claudeID := "claude-int-resume-1"

	// must have an existing session
	f1 := newCapFixture(t)
	if err := f1.run(NewSessionStartCmd(), "--claude-session-id", claudeID); err != nil {
		t.Fatalf("start: %v", err)
	}

	f2 := newCapFixture(t)
	err := f2.run(NewResumeContextCmd(), "--claude-session-id", claudeID)
	if err != nil {
		t.Fatalf("resume: %v stderr=%s", err, f2.stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(f2.stdout.Bytes(), &resp); err != nil {
		t.Fatalf("decode stdout: %v body=%q", err, f2.stdout.String())
	}
	if _, ok := resp["session"]; !ok {
		t.Fatalf("expected 'session' key, got %v", resp)
	}
	_ = env
}

func TestIntegration_InboxPoll(t *testing.T) {
	env := newIntEnv(t)
	f := newCapFixture(t)
	if err := f.runNested(NewInboxCmd(), "poll"); err != nil {
		t.Fatalf("inbox poll: %v stderr=%s", err, f.stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(f.stdout.Bytes(), &resp); err != nil {
		t.Fatalf("decode stdout: %v body=%q", err, f.stdout.String())
	}
	msgs, _ := resp["messages"].([]any)
	if len(msgs) != 0 {
		t.Fatalf("messages = %v, want empty", msgs)
	}
	_ = env
}

func TestIntegration_ProjectRegister(t *testing.T) {
	env := newIntEnv(t)
	f := newCapFixture(t)
	err := f.runNested(NewProjectCmd(), "register",
		"--slug", "new-proj",
		"--name", "New Project",
		"--default-branch", "main",
	)
	if err != nil {
		t.Fatalf("project register: %v stderr=%s", err, f.stderr.String())
	}
	// Verify the row landed.
	var name string
	err = env.store.Pool.QueryRow(context.Background(),
		`SELECT name FROM projects WHERE slug = $1`, "new-proj").Scan(&name)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if name != "New Project" {
		t.Fatalf("name = %q", name)
	}

	// Re-register with updated name — must be idempotent.
	f2 := newCapFixture(t)
	err = f2.runNested(NewProjectCmd(), "register",
		"--slug", "new-proj",
		"--name", "Renamed Project",
	)
	if err != nil {
		t.Fatalf("re-register: %v stderr=%s", err, f2.stderr.String())
	}
	err = env.store.Pool.QueryRow(context.Background(),
		`SELECT name FROM projects WHERE slug = $1`, "new-proj").Scan(&name)
	if err != nil {
		t.Fatalf("re-select: %v", err)
	}
	if name != "Renamed Project" {
		t.Fatalf("name not updated: %q", name)
	}
}

func TestIntegration_BestEffortOnSanitiserBlock(t *testing.T) {
	// Override sanitiser with a real pattern. We need to bring up a fresh
	// env so the patterns file has content.
	dsn := os.Getenv("AGENT_HUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_HUB_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"events", "agents"} {
		_, _ = st.Pool.Exec(ctx, "TRUNCATE TABLE "+tbl+" CASCADE")
	}

	name := "agent-blk"
	tok := "blk-token"
	hash, _ := bcrypt.GenerateFromPassword([]byte(tok), bcrypt.MinCost)
	_, err = st.Pool.Exec(ctx,
		`INSERT INTO agents (name, role, token_hash) VALUES ($1, 'operator', $2)`, name, string(hash))
	if err != nil {
		t.Fatal(err)
	}

	patternsFile := filepath.Join(t.TempDir(), "patterns.txt")
	_ = os.WriteFile(patternsFile, []byte("forbidden\n"), 0o600)
	san, _ := sanitiser.Load(patternsFile)

	mw := &auth.Middleware{Pool: st.Pool, AdminToken: "x"}
	app := &server.App{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:     st,
		Sanitiser: san,
		Auth:      mw,
	}
	router := server.NewRouter(app, nil)
	httpSrv := httptest.NewServer(router)
	t.Cleanup(httpSrv.Close)

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	_ = os.WriteFile(tokenPath, []byte(tok), 0o600)

	t.Setenv(config.EnvURL, httpSrv.URL)
	t.Setenv(config.EnvTokenFile, tokenPath)
	t.Setenv(config.EnvAgentName, name)
	t.Setenv(config.EnvAuditLog, filepath.Join(dir, "audit.log"))

	f := newCapFixture(t)
	err = f.runNested(NewEventCmd(), "emit",
		"--type", "decision.proposed",
		"--summary", "this has forbidden words",
	)
	if err != nil {
		t.Fatalf("best-effort: err should be nil, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "sanitiser blocked") {
		t.Fatalf("stderr = %q", f.stderr.String())
	}
}

// newCapFixture builds a minimal fixture with capture buffers but without
// the httptest server (the integration env stands up its own). Reuses the
// run + runNested helpers.
func newCapFixture(t *testing.T) *testFixture {
	t.Helper()
	return &testFixture{
		t:      t,
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
}

// silence unused-import warning if io drops out.
var _ = io.Discard
