package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// joinFixture stands up a mock gateway that routes the POST endpoints the
// join flow touches. Tests can inspect call counts and the register-agent
// body via the embedded testFixture.
type joinFixture struct {
	*testFixture
	mintCalls     int
	registerCalls int
	eventCalls    int
	startCalls    int
	endCalls      int
	resumeCalls   int
}

func newJoinFixture(t *testing.T) *joinFixture {
	t.Helper()
	jf := &joinFixture{}
	tf := &testFixture{
		t:              t,
		stdout:         &bytes.Buffer{},
		stderr:         &bytes.Buffer{},
		responseStatus: 200,
		responseBody:   `{"ok":true}`,
	}
	tf.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/admin/agents/") && strings.HasSuffix(r.URL.Path, "/mint-token"):
			jf.mintCalls++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"agent_id":"abc","name":"agent-test","token":"fresh-token-xyz"}`))
		case r.URL.Path == "/v1/agents/register":
			jf.registerCalls++
			body, _ := readAndDecode(r)
			tf.gotBody = body
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"agent-id"}`))
		case r.URL.Path == "/v1/sessions/start":
			jf.startCalls++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sess-id"}`))
		case r.URL.Path == "/v1/sessions/end":
			jf.endCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-id"}`))
		case r.URL.Path == "/v1/events":
			jf.eventCalls++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"event_id":"evt-id"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/resume-context"):
			jf.resumeCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"session":{"id":"x"},"recent_events":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found"}`))
		}
	}))
	t.Cleanup(tf.server.Close)

	// Override HOME so the join flow writes into a temp dir instead of the
	// developer's real ~/.config/concept-workflow/.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.EnvURL, tf.server.URL)
	t.Setenv(config.EnvAuditLog, filepath.Join(home, "audit.log"))

	jf.testFixture = tf
	return jf
}

// readAndDecode is a tiny helper for the mock server.
func readAndDecode(r *http.Request) (map[string]any, error) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

// =============================================================================
// flag validation
// =============================================================================

func TestJoin_RequiresProjectSlug(t *testing.T) {
	f := newJoinFixture(t)
	t.Setenv("AGENT_PROJECT_SLUG", "") // ensure empty

	err := f.run(NewJoinCmd(),
		"--name", "agent-1",
		"--alias", "Mikey",
		"--bootstrap-token", "env:ADMIN_TOK_NOT_SET",
	)
	if err != nil {
		t.Fatalf("best-effort: should exit 0 with stderr, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--project-slug is required") {
		t.Fatalf("stderr should explain the missing slug: %q", f.stderr.String())
	}
}

func TestJoin_RotateFlag_ForcesFreshMint(t *testing.T) {
	f := newJoinFixture(t)
	t.Setenv("AGENT_PROJECT_SLUG", "demo-project")
	t.Setenv("ADMIN_TOK_FOR_TEST", "test-admin-bearer")

	// Pre-seed an existing token file chmod 600.
	home := os.Getenv("HOME")
	cfgDir := filepath.Join(home, ".config", "concept-workflow")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(cfgDir, "agent-hub-token")
	if err := os.WriteFile(existing, []byte("OLD-VALUE"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without --rotate: mint is skipped.
	err := f.run(NewJoinCmd(),
		"--name", "agent-1",
		"--alias", "Mikey",
		"--project-slug", "demo-project",
		"--bootstrap-token", "env:ADMIN_TOK_FOR_TEST",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if f.mintCalls != 0 {
		t.Fatalf("expected no mint call without --rotate; got %d", f.mintCalls)
	}
	raw, _ := os.ReadFile(existing)
	if string(raw) != "OLD-VALUE" {
		t.Fatalf("token should be unchanged without --rotate; got %q", string(raw))
	}

	// With --rotate: mint fires.
	f.mintCalls = 0
	f.stderr.Reset()
	err = f.run(NewJoinCmd(),
		"--name", "agent-1",
		"--alias", "Mikey",
		"--project-slug", "demo-project",
		"--bootstrap-token", "env:ADMIN_TOK_FOR_TEST",
		"--rotate",
	)
	if err != nil {
		t.Fatalf("run rotate: %v stderr=%s", err, f.stderr.String())
	}
	if f.mintCalls != 1 {
		t.Fatalf("expected 1 mint call with --rotate; got %d", f.mintCalls)
	}
	raw, _ = os.ReadFile(existing)
	if string(raw) != "fresh-token-xyz" {
		t.Fatalf("token should be rotated; got %q", string(raw))
	}
}

func TestJoin_IdempotentRerun_SkipsMintWhenTokenPresent(t *testing.T) {
	f := newJoinFixture(t)
	t.Setenv("AGENT_PROJECT_SLUG", "demo-project")

	// Pre-seed an existing token file chmod 600.
	home := os.Getenv("HOME")
	cfgDir := filepath.Join(home, ".config", "concept-workflow")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(cfgDir, "agent-hub-token")
	if err := os.WriteFile(existing, []byte("EXISTING-VALUE"), 0o600); err != nil {
		t.Fatal(err)
	}

	// No bootstrap-token needed because we're not rotating.
	err := f.run(NewJoinCmd(),
		"--name", "agent-1",
		"--alias", "Mikey",
		"--project-slug", "demo-project",
		"--bootstrap-token", "/nonexistent-file-no-need",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if f.mintCalls != 0 {
		t.Fatalf("expected no mint call; got %d", f.mintCalls)
	}
	if !strings.Contains(f.stderr.String(), "token already present") {
		t.Fatalf("stderr should announce idempotent skip: %q", f.stderr.String())
	}
}

func TestJoin_WritesEnvFile(t *testing.T) {
	f := newJoinFixture(t)
	t.Setenv("AGENT_PROJECT_SLUG", "demo-project")
	t.Setenv("ADMIN_TOK_X", "admin-bearer")

	err := f.run(NewJoinCmd(),
		"--name", "agent-1",
		"--alias", "Mikey",
		"--project-slug", "demo-project",
		"--bootstrap-token", "env:ADMIN_TOK_X",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	home := os.Getenv("HOME")
	envPath := filepath.Join(home, ".config", "concept-workflow", "agent-events.env")
	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	for _, expect := range []string{
		"export AGENT_HUB_URL=",
		"export AGENT_HUB_TOKEN_FILE=",
		`export AGENT_NAME="agent-1"`,
		`export AGENT_PROJECT_SLUG="demo-project"`,
	} {
		if !strings.Contains(string(raw), expect) {
			t.Errorf("env file missing %q; body=\n%s", expect, string(raw))
		}
	}
	// File must be chmod 600 (no group/world bits).
	info, _ := os.Stat(envPath)
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("env file should be chmod 600; got %o", info.Mode().Perm())
	}
}

func TestJoin_SmokeRunsRoundTrip(t *testing.T) {
	f := newJoinFixture(t)
	t.Setenv("AGENT_PROJECT_SLUG", "demo-project")
	t.Setenv("ADMIN_TOK_S", "admin-bearer")

	err := f.run(NewJoinCmd(),
		"--name", "agent-1",
		"--alias", "Mikey",
		"--project-slug", "demo-project",
		"--bootstrap-token", "env:ADMIN_TOK_S",
		"--smoke",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if f.startCalls != 1 || f.eventCalls != 1 || f.resumeCalls != 1 || f.endCalls != 1 {
		t.Fatalf("smoke round-trip incomplete: start=%d event=%d resume=%d end=%d",
			f.startCalls, f.eventCalls, f.resumeCalls, f.endCalls)
	}
	if !strings.Contains(f.stderr.String(), "smoke ok") {
		t.Fatalf("stderr should report smoke ok; got %q", f.stderr.String())
	}
}

func TestJoin_RegisterAgentSendsAlias(t *testing.T) {
	f := newJoinFixture(t)
	t.Setenv("AGENT_PROJECT_SLUG", "demo-project")
	t.Setenv("ADMIN_TOK_A", "admin-bearer")

	err := f.run(NewJoinCmd(),
		"--name", "agent-1",
		"--alias", "Mikey",
		"--project-slug", "demo-project",
		"--bootstrap-token", "env:ADMIN_TOK_A",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if f.registerCalls != 1 {
		t.Fatalf("expected one register call; got %d", f.registerCalls)
	}
	if f.gotBody["mattermost_username"] != "Mikey" {
		t.Fatalf("alias should reach gateway as mattermost_username; got %v", f.gotBody["mattermost_username"])
	}
	if f.gotBody["name"] != "agent-1" {
		t.Fatalf("name = %v", f.gotBody["name"])
	}
}

func TestJoin_RejectsInsecureBootstrapTokenFile(t *testing.T) {
	f := newJoinFixture(t)
	t.Setenv("AGENT_PROJECT_SLUG", "demo-project")
	dir := t.TempDir()
	tok := filepath.Join(dir, "bad-perms")
	if err := os.WriteFile(tok, []byte("admin-bearer"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := f.run(NewJoinCmd(),
		"--name", "agent-1",
		"--alias", "Mikey",
		"--project-slug", "demo-project",
		"--bootstrap-token", tok,
	)
	if err != nil {
		t.Fatalf("best-effort: should exit 0 with stderr, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "insecure perms") {
		t.Fatalf("stderr should warn on insecure perms: %q", f.stderr.String())
	}
}

func TestJoin_SmokeSessionIDIsUnique(t *testing.T) {
	// bug #30 fix: two consecutive runs with the same agent must not
	// collide on the smoke session-id. We can't easily inspect the body
	// shape from this mock so we sniff the resume-context path which
	// embeds the session-id.
	f := newJoinFixture(t)
	t.Setenv("AGENT_PROJECT_SLUG", "demo-project")
	t.Setenv("ADMIN_TOK_U", "admin-bearer")

	// Capture session-ids seen by the resume endpoint.
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/admin/agents/") && strings.HasSuffix(r.URL.Path, "/mint-token"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"agent_id":"abc","name":"agent-test","token":"fresh-token"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/resume-context"):
			// Extract the session-id segment.
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) >= 4 {
				seen = append(seen, parts[3])
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"session":{"id":"x"}}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvURL, srv.URL)

	for i := 0; i < 2; i++ {
		// Force a fresh mint each run so the smoke fires both times.
		err := f.run(NewJoinCmd(),
			"--name", "agent-1",
			"--alias", "Mikey",
			"--project-slug", "demo-project",
			"--bootstrap-token", "env:ADMIN_TOK_U",
			"--smoke",
			"--rotate",
		)
		if err != nil {
			t.Fatalf("run %d: %v stderr=%s", i, err, f.stderr.String())
		}
	}
	if len(seen) < 2 {
		t.Fatalf("expected 2 resume-context calls; got %d (seen=%v)", len(seen), seen)
	}
	if seen[0] == seen[1] {
		t.Fatalf("smoke session-ids must differ between runs; both were %q", seen[0])
	}
}
