package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// testFixture wires the env vars + a httptest gateway + a captured-IO root
// command. Each test resets env in t.Setenv so they're isolated.
type testFixture struct {
	t          *testing.T
	server     *httptest.Server
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	auditPath  string
	gotMethod  string
	gotPath    string
	gotBody    map[string]any
	gotRawBody string
	gotAuth    string
	gotQuery   string

	// responseStatus + responseBody control what the test server replies.
	responseStatus int
	responseBody   string
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()
	f := &testFixture{
		t:              t,
		stdout:         &bytes.Buffer{},
		stderr:         &bytes.Buffer{},
		responseStatus: 200,
		responseBody:   `{"ok":true}`,
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotMethod = r.Method
		f.gotPath = r.URL.Path
		f.gotAuth = r.Header.Get("Authorization")
		f.gotQuery = r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		f.gotRawBody = string(raw)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &f.gotBody)
		}
		w.WriteHeader(f.responseStatus)
		_, _ = w.Write([]byte(f.responseBody))
	}))
	t.Cleanup(f.server.Close)

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.auditPath = filepath.Join(dir, "audit.log")

	t.Setenv(config.EnvURL, f.server.URL)
	t.Setenv(config.EnvTokenFile, tokenPath)
	t.Setenv(config.EnvAgentName, "agent-test")
	t.Setenv(config.EnvProjectSlug, "demo-project")
	// v0.1.17 — disable the resolveClaudeSessionID file fallback by default.
	// Without this, any cached ~/.cache/concept-workflow/claude-session-id
	// on a developer's machine would leak into tests that expect empty
	// session id semantics ("no flag, no env"). Tests that specifically
	// exercise the file fallback override CLAUDE_SESSION_ID_FILE themselves.
	t.Setenv(claudeSessionIDFileEnv, filepath.Join(dir, "no-such-csid-file"))
	t.Setenv(config.EnvAuditLog, f.auditPath)
	return f
}

// run wires the given subcommand under a fresh root (so persistent --strict
// is registered), drives Execute with args, and returns the resulting error.
func (f *testFixture) run(sub *cobra.Command, args ...string) error {
	root := &cobra.Command{Use: "agentctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("strict", false, "")
	root.AddCommand(sub)
	root.SetOut(f.stdout)
	root.SetErr(f.stderr)
	// Cobra requires the subcommand name as the first arg.
	root.SetArgs(append([]string{sub.Name()}, args...))
	root.SetContext(context.Background())
	return root.Execute()
}

// runNested handles `agentctl event emit ...` shape (group + leaf).
func (f *testFixture) runNested(group *cobra.Command, leafName string, args ...string) error {
	root := &cobra.Command{Use: "agentctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("strict", false, "")
	root.AddCommand(group)
	root.SetOut(f.stdout)
	root.SetErr(f.stderr)
	root.SetArgs(append([]string{group.Name(), leafName}, args...))
	root.SetContext(context.Background())
	return root.Execute()
}

// =============================================================================
// register-agent
// =============================================================================

func TestRegisterAgent_BuildsCorrectRequest(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"id":"abc","name":"agent-test"}`

	err := f.run(NewRegisterAgentCmd(),
		"--role", "operator",
		"--host-kind", "mac-host",
		"--vm-hostname", "macbook",
		"--capabilities", "claude,git",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotMethod != "POST" || f.gotPath != "/v1/agents/register" {
		t.Fatalf("method/path = %s %s", f.gotMethod, f.gotPath)
	}
	if f.gotBody["name"] != "agent-test" {
		t.Fatalf("name = %v", f.gotBody["name"])
	}
	if f.gotBody["role"] != "operator" {
		t.Fatalf("role = %v", f.gotBody["role"])
	}
	caps, _ := f.gotBody["capabilities"].([]any)
	if len(caps) != 2 {
		t.Fatalf("capabilities = %v", caps)
	}
	if f.gotAuth != "Bearer test-token" {
		t.Fatalf("auth = %q", f.gotAuth)
	}
	// Stderr should have the success summary.
	if !strings.Contains(f.stderr.String(), "register-agent: agent agent-test registered") {
		t.Fatalf("stderr = %q", f.stderr.String())
	}
}

func TestRegisterAgent_SerializesMattermostUsername(t *testing.T) {
	// v0.1.2 completion: the --mattermost-username flag must reach the
	// gateway as `mattermost_username` so agent aliases (Splinter / Mikey /
	// Donnie) post under the right MM handle.
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"id":"abc","name":"agent-test"}`

	err := f.run(NewRegisterAgentCmd(),
		"--mattermost-username", "splinter",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotBody["mattermost_username"] != "splinter" {
		t.Fatalf("mattermost_username = %v, want 'splinter'; raw body: %s",
			f.gotBody["mattermost_username"], f.gotRawBody)
	}
}

func TestRegisterAgent_RejectsNameMismatch(t *testing.T) {
	f := newFixture(t)
	err := f.run(NewRegisterAgentCmd(), "--name", "other-agent")
	if err != nil {
		t.Fatalf("best-effort default: err should be nil, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "must equal AGENT_NAME") {
		t.Fatalf("stderr = %q", f.stderr.String())
	}
}

func TestRegisterAgent_StrictNameMismatchReturnsError(t *testing.T) {
	f := newFixture(t)
	err := f.run(NewRegisterAgentCmd(), "--strict", "--name", "other-agent")
	if err == nil {
		t.Fatal("strict mode should propagate error")
	}
}

// =============================================================================
// session-start
// =============================================================================

func TestSessionStart_BuildsCorrectRequest(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"id":"sess-uuid"}`

	err := f.run(NewSessionStartCmd(),
		"--claude-session-id", "claude-abc-123",
		"--branch", "feat/x",
		"--start-reason", "test",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotPath != "/v1/sessions/start" {
		t.Fatalf("path = %s", f.gotPath)
	}
	if f.gotBody["claude_session_id"] != "claude-abc-123" {
		t.Fatalf("claude_session_id = %v", f.gotBody["claude_session_id"])
	}
	if f.gotBody["project_slug"] != "demo-project" {
		t.Fatalf("project_slug should default to AGENT_PROJECT_SLUG, got %v", f.gotBody["project_slug"])
	}
	if f.gotBody["branch"] != "feat/x" {
		t.Fatalf("branch = %v", f.gotBody["branch"])
	}
}

func TestSessionStart_RequiresClaudeSessionID(t *testing.T) {
	// HIGH #2 fix: arg-validation failures used to silently exit 1 (cobra's
	// SilenceErrors:true swallowed them). They now go through validationError
	// which respects the best-effort/strict posture and always writes stderr.
	f := newFixture(t)
	err := f.run(NewSessionStartCmd())
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0 with stderr, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--claude-session-id is required") {
		t.Fatalf("stderr should explain the validation failure: %q", f.stderr.String())
	}
	if !strings.Contains(f.stderr.String(), "continuing (best-effort)") {
		t.Fatalf("stderr should mark posture: %q", f.stderr.String())
	}
}

func TestSessionStart_RequiresClaudeSessionID_StrictPropagates(t *testing.T) {
	f := newFixture(t)
	err := f.run(NewSessionStartCmd(), "--strict")
	if err == nil {
		t.Fatal("strict mode: validation should propagate as error")
	}
	if !strings.Contains(f.stderr.String(), "halting (--strict)") {
		t.Fatalf("stderr should mark strict halt: %q", f.stderr.String())
	}
}

func TestSessionStart_ValidationFailureIsAudited(t *testing.T) {
	// HIGH #2 fix: arg-validation failures must produce an audit entry so
	// the operator can correlate a missing-flag exit with the call site.
	f := newFixture(t)
	if err := f.run(NewSessionStartCmd()); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(f.auditPath)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(raw), `"outcome":"validation_error"`) {
		t.Fatalf("audit log should record validation_error: %q", string(raw))
	}
	if !strings.Contains(string(raw), `"command":"session-start"`) {
		t.Fatalf("audit log should name the command: %q", string(raw))
	}
}

// =============================================================================
// session-end
// =============================================================================

func TestSessionEnd_BuildsCorrectRequest(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"id":"sess-uuid"}`

	err := f.run(NewSessionEndCmd(),
		"--claude-session-id", "claude-abc-123",
		"--final-status", "task_completed",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotPath != "/v1/sessions/end" {
		t.Fatalf("path = %s", f.gotPath)
	}
	if f.gotBody["final_status"] != "task_completed" {
		t.Fatalf("final_status = %v", f.gotBody["final_status"])
	}
}

// =============================================================================
// event emit
// =============================================================================

func TestEventEmit_BuildsCorrectRequest(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-uuid-xxxxxxx"}`

	err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--summary", "smoke",
		"--json-payload", `{"phase":"spec"}`,
		"--task-key", "feat-01",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotPath != "/v1/events" {
		t.Fatalf("path = %s", f.gotPath)
	}
	if f.gotBody["event_type"] != "progress.updated" {
		t.Fatalf("event_type = %v", f.gotBody["event_type"])
	}
	payload, _ := f.gotBody["payload"].(map[string]any)
	if payload["phase"] != "spec" {
		t.Fatalf("payload = %v", f.gotBody["payload"])
	}
	if f.gotBody["task_key"] != "feat-01" {
		t.Fatalf("task_key = %v", f.gotBody["task_key"])
	}
}

// v0.1.11 — same agent_session_id tagging fix as improvement emit. event_emit
// already had the --claude-session-id flag pre-v0.1.11; v0.1.11 adds the
// $CLAUDE_SESSION_ID env fallback so unflagged emits in tool contexts also
// get tagged. Empty-resolution warning is identical to improvement emit.
func TestEventEmit_ClaudeSessionIDFromEnv(t *testing.T) {
	f := newFixture(t)
	t.Setenv("CLAUDE_SESSION_ID", "claude-env-eemit-1")
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-1"}`

	err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--summary", "smoke",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotBody["claude_session_id"] != "claude-env-eemit-1" {
		t.Fatalf("claude_session_id should default to $CLAUDE_SESSION_ID; got %v",
			f.gotBody["claude_session_id"])
	}
	if strings.Contains(f.stderr.String(), "session-orphaned") {
		t.Fatalf("stderr should NOT warn when env is set; got %q", f.stderr.String())
	}
}

// v0.1.17 — file fallback for resolveClaudeSessionID. The SessionStart hook
// writes the current session id to ~/.cache/concept-workflow/claude-session-id
// (or whatever $CLAUDE_SESSION_ID_FILE points to) so bash subshells spawned
// by Claude Code's Bash tool — which don't reliably inherit env vars from
// the hook context — can still resolve the id without needing the explicit
// --claude-session-id flag on every call.
//
// Precedence: flag → env → file → empty.

func TestResolveClaudeSessionID_FlagWinsOverEverything(t *testing.T) {
	tmp := t.TempDir()
	idfile := filepath.Join(tmp, "csid")
	if err := os.WriteFile(idfile, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SESSION_ID", "from-env")
	t.Setenv("CLAUDE_SESSION_ID_FILE", idfile)
	if got := resolveClaudeSessionID("from-flag"); got != "from-flag" {
		t.Fatalf("flag must win; got %q", got)
	}
}

func TestResolveClaudeSessionID_EnvWinsOverFile(t *testing.T) {
	tmp := t.TempDir()
	idfile := filepath.Join(tmp, "csid")
	if err := os.WriteFile(idfile, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SESSION_ID", "from-env")
	t.Setenv("CLAUDE_SESSION_ID_FILE", idfile)
	if got := resolveClaudeSessionID(""); got != "from-env" {
		t.Fatalf("env must win over file; got %q", got)
	}
}

func TestResolveClaudeSessionID_FallsBackToFile(t *testing.T) {
	tmp := t.TempDir()
	idfile := filepath.Join(tmp, "csid")
	if err := os.WriteFile(idfile, []byte("from-file-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SESSION_ID", "")
	t.Setenv("CLAUDE_SESSION_ID_FILE", idfile)
	if got := resolveClaudeSessionID(""); got != "from-file-123" {
		t.Fatalf("should read from file; got %q", got)
	}
}

func TestResolveClaudeSessionID_EmptyWhenAllUnset(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_SESSION_ID", "")
	t.Setenv("CLAUDE_SESSION_ID_FILE", filepath.Join(tmp, "nope-does-not-exist"))
	if got := resolveClaudeSessionID(""); got != "" {
		t.Fatalf("all-empty must resolve empty; got %q", got)
	}
}

func TestEventEmit_NoSessionIDWarnsButProceeds(t *testing.T) {
	f := newFixture(t)
	t.Setenv("CLAUDE_SESSION_ID", "")
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-1"}`

	err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--summary", "orphan",
	)
	if err != nil {
		t.Fatalf("best-effort: missing session id should not halt; got %v", err)
	}
	if _, ok := f.gotBody["claude_session_id"]; ok {
		t.Fatalf("body should omit claude_session_id when unresolved; raw=%s", f.gotRawBody)
	}
	if !strings.Contains(f.stderr.String(), "event emit: no CLAUDE_SESSION_ID") {
		t.Fatalf("stderr should warn about orphaned event; got %q", f.stderr.String())
	}
}

func TestEventEmit_SanitiserBlockedBestEffortExits0(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 422
	f.responseBody = `{"error":"sanitiser_blocked","message":"blocked","matched_pattern":"\\b10\\.\\d+\\.\\d+\\.\\d+\\b","matched_field":"summary","blocked_event_id":"audit-id-7"}`

	err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--summary", "leaks 10.0.0.1",
	)
	if err != nil {
		t.Fatalf("best-effort default: err should be nil, got %v", err)
	}
	stderr := f.stderr.String()
	if !strings.Contains(stderr, "sanitiser blocked") {
		t.Fatalf("stderr = %q", stderr)
	}
	// BLOCKER #1 fix: the stderr line MUST surface which §2.1 pattern fired
	// and which field tripped it, so the operator can fix the offending
	// content without re-fetching the gateway audit log. The fields are
	// top-level JSON on the gateway's 422 response and must be decoded by
	// ErrorEnvelope (not stuffed into Details).
	// %q re-escapes backslashes, so the literal `\b` in the pattern shows up
	// as `\\b` in the stderr line. Match what the formatter actually emits.
	if !strings.Contains(stderr, `matched_pattern="\\b10\\.\\d+\\.\\d+\\.\\d+\\b"`) {
		t.Errorf("stderr missing matched_pattern; got: %s", stderr)
	}
	if !strings.Contains(stderr, "matched_field=summary") {
		t.Errorf("stderr missing matched_field; got: %s", stderr)
	}
	if !strings.Contains(stderr, "blocked_event_id=audit-id-7") {
		t.Errorf("stderr missing blocked_event_id; got: %s", stderr)
	}
}

func TestEventEmit_SanitiserBlockedStrictExits1(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 422
	f.responseBody = `{"error":"sanitiser_blocked","message":"blocked"}`

	err := f.runNested(NewEventCmd(), "emit",
		"--strict",
		"--type", "progress.updated",
		"--summary", "leaks 10.0.0.1",
	)
	if err == nil {
		t.Fatal("strict mode should propagate")
	}
	if !strings.Contains(f.stderr.String(), "halting (--strict)") {
		t.Fatalf("stderr = %q", f.stderr.String())
	}
}

func TestEventEmit_JSONFlagWritesToStdout(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-1"}`

	err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--json",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(f.stdout.String(), "evt-1") {
		t.Fatalf("stdout = %q", f.stdout.String())
	}
}

// =============================================================================
// checkpoint
// =============================================================================

func TestCheckpoint_BuildsRepeatableFlagArrays(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"id":"ckpt-uuid"}`

	err := f.run(NewCheckpointCmd(),
		"--claude-session-id", "claude-abc",
		"--summary", "midway",
		"--next", "do A",
		"--next", "do B",
		"--open-question", "why?",
		"--risk", "data loss",
		"--file-relevant", "main.go",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotPath != "/v1/sessions/checkpoint" {
		t.Fatalf("path = %s", f.gotPath)
	}
	next, _ := f.gotBody["next_actions"].([]any)
	if len(next) != 2 || next[0] != "do A" || next[1] != "do B" {
		t.Fatalf("next_actions = %v", next)
	}
	oq, _ := f.gotBody["open_questions"].([]any)
	if len(oq) != 1 || oq[0] != "why?" {
		t.Fatalf("open_questions = %v", oq)
	}
	risks, _ := f.gotBody["risks"].([]any)
	if len(risks) != 1 {
		t.Fatalf("risks = %v", risks)
	}
}

// =============================================================================
// resume-context
// =============================================================================

func TestResumeContext_GETsCorrectPath(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"session":{"id":"x"},"recent_events":[]}`

	err := f.run(NewResumeContextCmd(),
		"--claude-session-id", "claude-abc-123",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotMethod != "GET" {
		t.Fatalf("method = %s", f.gotMethod)
	}
	if f.gotPath != "/v1/sessions/claude-abc-123/resume-context" {
		t.Fatalf("path = %s", f.gotPath)
	}
	// Default rendering should emit JSON on stdout.
	if !strings.Contains(f.stdout.String(), "session") {
		t.Fatalf("stdout = %q", f.stdout.String())
	}
}

func TestResumeContext_FallsBackToCLAUDESESSIONIDEnv(t *testing.T) {
	f := newFixture(t)
	t.Setenv("CLAUDE_SESSION_ID", "from-env-123")
	f.responseStatus = 200
	f.responseBody = `{}`

	err := f.run(NewResumeContextCmd())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotPath != "/v1/sessions/from-env-123/resume-context" {
		t.Fatalf("path = %s; expected env fallback", f.gotPath)
	}
}

func TestResumeContext_PrettyFlagIndents(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"a":1,"b":2}`
	err := f.run(NewResumeContextCmd(),
		"--claude-session-id", "x",
		"--pretty",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(f.stdout.String(), "\n  ") {
		t.Fatalf("stdout should be indented: %q", f.stdout.String())
	}
}

// resume-context — v0.1.12 no-flag fallback + --prior cases. These use a
// fresh httptest.Server inside the test (not the single-call fixture) because
// the fallback path makes TWO sequential calls:
//   1) GET /v1/me/latest-session  (self-scoped; v0.1.13 endpoint)
//   2) GET /v1/sessions/{id}/resume-context
// We need per-path response routing + call-sequence assertion that the
// single-shot fixture can't express.

func TestResumeContext_NoFlag_FallsBackToLatestSession(t *testing.T) {
	t.Setenv("CLAUDE_SESSION_ID", "") // explicit: no current session

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/v1/me/latest-session":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"agent_id":"a-1","agent_name":"agent-test","latest_session":{"claude_session_id":"prior-session-99","status":"ended","started_at":"2026-05-23T10:00:00Z"}}`))
		case "/v1/sessions/prior-session-99/resume-context":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"session":{"id":"a"}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not_found","path":"` + r.URL.Path + `"}`))
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvTokenFile, tokenPath)
	t.Setenv(config.EnvAgentName, "agent-test")
	t.Setenv(config.EnvProjectSlug, "demo-project")
	t.Setenv(config.EnvAuditLog, filepath.Join(dir, "audit.log"))

	root := &cobra.Command{Use: "agentctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("strict", false, "")
	root.AddCommand(NewResumeContextCmd())
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"resume-context"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d: %v", len(paths), paths)
	}
	if !strings.HasPrefix(paths[0], "/v1/me/latest-session") {
		t.Fatalf("call 1 path = %s", paths[0])
	}
	// No --prior → no ?exclude
	if strings.Contains(paths[0], "exclude=") {
		t.Fatalf("call 1 should NOT have exclude=: %s", paths[0])
	}
	if !strings.HasPrefix(paths[1], "/v1/sessions/prior-session-99/resume-context") {
		t.Fatalf("call 2 path = %s", paths[1])
	}
}

func TestResumeContext_PriorFlag_PassesExcludeFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_SESSION_ID", "current-new-session")

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/v1/me/latest-session":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"latest_session":{"claude_session_id":"prior-session-77"}}`))
		case "/v1/sessions/prior-session-77/resume-context":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"session":{"id":"a"}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvTokenFile, tokenPath)
	t.Setenv(config.EnvAgentName, "agent-test")
	t.Setenv(config.EnvProjectSlug, "demo-project")
	t.Setenv(config.EnvAuditLog, filepath.Join(dir, "audit.log"))

	root := &cobra.Command{Use: "agentctl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("strict", false, "")
	root.AddCommand(NewResumeContextCmd())
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"resume-context", "--prior"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d: %v", len(paths), paths)
	}
	if !strings.Contains(paths[0], "exclude=current-new-session") {
		t.Fatalf("call 1 should pass exclude=current-new-session: %s", paths[0])
	}
	if !strings.HasPrefix(paths[1], "/v1/sessions/prior-session-77/resume-context") {
		t.Fatalf("call 2 path = %s", paths[1])
	}
}

func TestResumeContext_ExplicitFlagSkipsFallback(t *testing.T) {
	// Even with empty env, an explicit flag must bypass the fallback entirely.
	f := newFixture(t)
	t.Setenv("CLAUDE_SESSION_ID", "")
	f.responseStatus = 200
	f.responseBody = `{"session":{"id":"x"}}`

	err := f.run(NewResumeContextCmd(), "--claude-session-id", "explicit-abc")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Single call, straight to resume-context — no latest-session lookup.
	if f.gotPath != "/v1/sessions/explicit-abc/resume-context" {
		t.Fatalf("path = %s (fallback path leaked into explicit-flag invocation)", f.gotPath)
	}
}

// =============================================================================
// checkpoint — v0.1.12 env-fallback parity with improvement/event emit
// =============================================================================

func TestCheckpoint_FallsBackToCLAUDESESSIONIDEnv(t *testing.T) {
	f := newFixture(t)
	t.Setenv("CLAUDE_SESSION_ID", "from-env-checkpoint-1")
	f.responseStatus = 201
	f.responseBody = `{"id":"ckpt-uuid"}`

	err := f.run(NewCheckpointCmd(),
		"--summary", "midway via env fallback",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotBody["claude_session_id"] != "from-env-checkpoint-1" {
		t.Fatalf("expected env-fallback claude_session_id, got %v", f.gotBody["claude_session_id"])
	}
}

func TestCheckpoint_FlagWinsOverEnv(t *testing.T) {
	f := newFixture(t)
	t.Setenv("CLAUDE_SESSION_ID", "from-env")
	f.responseStatus = 201
	f.responseBody = `{"id":"ckpt-uuid"}`

	err := f.run(NewCheckpointCmd(),
		"--claude-session-id", "from-flag",
		"--summary", "explicit flag wins",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotBody["claude_session_id"] != "from-flag" {
		t.Fatalf("expected flag to win, got %v", f.gotBody["claude_session_id"])
	}
}

// =============================================================================
// inbox poll
// =============================================================================

func TestInboxPoll_UsesAgentNameFromEnv(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"agent_name":"agent-test","messages":[]}`

	err := f.runNested(NewInboxCmd(), "poll")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotPath != "/v1/inbox" {
		t.Fatalf("path = %s", f.gotPath)
	}
	if !strings.Contains(f.gotQuery, "agent_name=agent-test") {
		t.Fatalf("query = %s", f.gotQuery)
	}
}

func TestInboxPoll_AcceptsSinceFlag(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"messages":[]}`

	err := f.runNested(NewInboxCmd(), "poll",
		"--since", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(f.gotQuery, "since=2024-01-01") {
		t.Fatalf("query = %s", f.gotQuery)
	}
}

func TestInboxPoll_RejectsInvalidSince(t *testing.T) {
	// HIGH #2 fix: invalid --since used to silently exit 1; now goes through
	// validationError so best-effort exits 0 with stderr, strict propagates.
	f := newFixture(t)
	err := f.runNested(NewInboxCmd(), "poll", "--since", "not-a-date")
	if err != nil {
		t.Fatalf("best-effort default: should exit 0 with stderr, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--since") {
		t.Fatalf("stderr should explain the parse failure: %q", f.stderr.String())
	}
}

func TestInboxPoll_RejectsInvalidSince_StrictPropagates(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewInboxCmd(), "poll", "--strict", "--since", "not-a-date")
	if err == nil {
		t.Fatal("strict mode: invalid --since should propagate")
	}
}

// =============================================================================
// health
// =============================================================================

func TestHealth_HitsHealthEndpoint(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"status":"ok","sanitiser_patterns":5}`

	err := f.run(NewHealthCmd())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotPath != "/health" {
		t.Fatalf("path = %s", f.gotPath)
	}
	if f.gotAuth != "" {
		t.Fatalf("auth = %q, want empty (health is no-auth)", f.gotAuth)
	}
	if !strings.Contains(f.stdout.String(), "status") {
		t.Fatalf("stdout = %q", f.stdout.String())
	}
}

func TestHealth_AlwaysStrictRegardlessOfFlag(t *testing.T) {
	// Even without --strict, a 500 should propagate.
	f := newFixture(t)
	f.responseStatus = 500
	f.responseBody = `{"error":"postgres_unreachable"}`

	err := f.run(NewHealthCmd())
	if err == nil {
		t.Fatal("health must always exit non-zero on failure")
	}
}

// =============================================================================
// best-effort posture cross-cutting
// =============================================================================

func TestBestEffort_NetworkErrorExits0WithStderr(t *testing.T) {
	f := newFixture(t)
	// Point at a closed port — config still loads, request fails.
	t.Setenv(config.EnvURL, "http://127.0.0.1:1")
	err := f.run(NewSessionEndCmd(), "--claude-session-id", "x")
	if err != nil {
		t.Fatalf("best-effort: err should be nil, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "continuing (best-effort)") {
		t.Fatalf("stderr = %q", f.stderr.String())
	}
}

func TestBestEffort_StrictModeNetworkErrorExits1(t *testing.T) {
	f := newFixture(t)
	t.Setenv(config.EnvURL, "http://127.0.0.1:1")
	err := f.run(NewSessionEndCmd(), "--strict", "--claude-session-id", "x")
	if err == nil {
		t.Fatal("strict mode should propagate")
	}
}

func TestAudit_LogIsWrittenOnSuccess(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"id":"x"}`
	err := f.run(NewSessionEndCmd(), "--claude-session-id", "abc")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(f.auditPath)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(raw), `"command":"session-end"`) {
		t.Fatalf("audit log = %q", string(raw))
	}
	if !strings.Contains(string(raw), `"outcome":"ok"`) {
		t.Fatalf("audit log missing ok outcome: %q", string(raw))
	}
}

// =============================================================================
// project register
// =============================================================================

func TestProjectRegister_BuildsCorrectRequest(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"id":"proj-uuid-abcdefgh","slug":"secureup","name":"Secureup"}`

	err := f.runNested(NewProjectCmd(), "register",
		"--slug", "secureup",
		"--name", "Secureup",
		"--forge-url", "ssh://git@forge:2222/x/workspace.git",
		"--default-branch", "develop",
		"--mattermost-outbox-channel", "agent-events",
		"--mattermost-inbox-channel", "agent-inbox",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%q", err, f.stderr.String())
	}
	if f.gotMethod != "POST" || f.gotPath != "/v1/projects" {
		t.Fatalf("method/path = %s %s", f.gotMethod, f.gotPath)
	}
	if f.gotBody["slug"] != "secureup" {
		t.Fatalf("slug = %v", f.gotBody["slug"])
	}
	if f.gotBody["name"] != "Secureup" {
		t.Fatalf("name = %v", f.gotBody["name"])
	}
	if f.gotBody["forge_url"] != "ssh://git@forge:2222/x/workspace.git" {
		t.Fatalf("forge_url = %v", f.gotBody["forge_url"])
	}
	if f.gotBody["default_branch"] != "develop" {
		t.Fatalf("default_branch = %v", f.gotBody["default_branch"])
	}
	if f.gotBody["mattermost_outbox_channel"] != "agent-events" {
		t.Fatalf("outbox = %v", f.gotBody["mattermost_outbox_channel"])
	}
	if f.gotBody["mattermost_inbox_channel"] != "agent-inbox" {
		t.Fatalf("inbox = %v", f.gotBody["mattermost_inbox_channel"])
	}
	// Summary on stderr with short id.
	if !strings.Contains(f.stderr.String(), "project register: project secureup registered (id=proj-uui...)") {
		t.Fatalf("stderr summary missing or wrong: %q", f.stderr.String())
	}
}

func TestProjectRegister_OnlyRequiredFields(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"id":"x","slug":"min","name":"Min"}`

	err := f.runNested(NewProjectCmd(), "register",
		"--slug", "min",
		"--name", "Min",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Optional fields must NOT be present in the body when unset.
	for _, key := range []string{"forge_url", "default_branch", "mattermost_outbox_channel", "mattermost_inbox_channel"} {
		if _, ok := f.gotBody[key]; ok {
			t.Errorf("body should omit %s when flag unset; raw=%s", key, f.gotRawBody)
		}
	}
}

func TestProjectRegister_RequiresSlug(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewProjectCmd(), "register", "--name", "X")
	if err != nil {
		t.Fatalf("best-effort: should exit 0 with stderr, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--slug is required") {
		t.Fatalf("stderr should explain the missing slug: %q", f.stderr.String())
	}
	if !strings.Contains(f.stderr.String(), "continuing (best-effort)") {
		t.Fatalf("stderr should mark posture: %q", f.stderr.String())
	}
}

func TestProjectRegister_RequiresName(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewProjectCmd(), "register", "--slug", "x")
	if err != nil {
		t.Fatalf("best-effort: should exit 0 with stderr, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--name is required") {
		t.Fatalf("stderr: %q", f.stderr.String())
	}
}

func TestProjectRegister_StrictPropagatesValidationError(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewProjectCmd(), "register", "--strict", "--name", "X")
	if err == nil {
		t.Fatal("strict mode: missing --slug should propagate")
	}
	if !strings.Contains(f.stderr.String(), "halting (--strict)") {
		t.Fatalf("stderr: %q", f.stderr.String())
	}
}

func TestProjectRegister_BestEffortOnNetworkError(t *testing.T) {
	f := newFixture(t)
	t.Setenv(config.EnvURL, "http://127.0.0.1:1")
	err := f.runNested(NewProjectCmd(), "register",
		"--slug", "x",
		"--name", "X",
	)
	if err != nil {
		t.Fatalf("best-effort: err should be nil, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "continuing (best-effort)") {
		t.Fatalf("stderr: %q", f.stderr.String())
	}
}

func TestProjectRegister_JSONFlagWritesToStdout(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 200
	f.responseBody = `{"id":"p1","slug":"x","name":"X"}`

	err := f.runNested(NewProjectCmd(), "register",
		"--slug", "x",
		"--name", "X",
		"--json",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(f.stdout.String(), `"slug":"x"`) {
		t.Fatalf("stdout should carry response body: %q", f.stdout.String())
	}
}

func TestAudit_LogIsWrittenOnError(t *testing.T) {
	f := newFixture(t)
	t.Setenv(config.EnvURL, "http://127.0.0.1:1")
	_ = f.run(NewSessionEndCmd(), "--claude-session-id", "abc")
	raw, err := os.ReadFile(f.auditPath)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(raw), `"outcome":"error"`) {
		t.Fatalf("audit log missing error outcome: %q", string(raw))
	}
}
