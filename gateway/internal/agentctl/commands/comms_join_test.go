package commands

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/commands/comms_backends"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// fakeBackend records all calls and replays canned responses. Tests swap it
// into commsBackendFactory.
type fakeBackend struct {
	mu             sync.Mutex
	validateCalls  int
	ensureCalls    int
	teamMemberCalls int
	addToChCalls   int
	mintCalls      int
	validateErr    error
	ensureErr      error
	teamMemberErr  error
	addErr         error
	mintErr        error
	lastBotName    string
	lastChannel    string
	lastBotID      string
	mintedPAT      string
	callOrder      []string
}

func (b *fakeBackend) Validate(_ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.validateCalls++
	b.callOrder = append(b.callOrder, "validate")
	return b.validateErr
}

func (b *fakeBackend) EnsureBotUser(_ string, name string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureCalls++
	b.callOrder = append(b.callOrder, "ensure-bot")
	b.lastBotName = name
	if b.ensureErr != nil {
		return "", b.ensureErr
	}
	return "bot-id-xyz", nil
}

func (b *fakeBackend) EnsureTeamMember(_ string, botID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.teamMemberCalls++
	b.callOrder = append(b.callOrder, "ensure-team-member")
	b.lastBotID = botID
	return b.teamMemberErr
}

func (b *fakeBackend) AddBotToChannel(_, botID, channel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addToChCalls++
	b.callOrder = append(b.callOrder, "add-to-channel")
	b.lastBotID = botID
	b.lastChannel = channel
	return b.addErr
}

func (b *fakeBackend) MintPAT(_, botID string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mintCalls++
	if b.mintErr != nil {
		return "", b.mintErr
	}
	if b.mintedPAT == "" {
		b.mintedPAT = "fresh-pat-value"
	}
	return b.mintedPAT, nil
}

func withFakeBackend(t *testing.T, fb *fakeBackend) {
	t.Helper()
	old := commsBackendFactory
	commsBackendFactory = func(name string) (comms_backends.Backend, error) {
		if name != "mattermost" {
			return nil, fmt.Errorf("fake factory only knows mattermost; got %s", name)
		}
		return fb, nil
	}
	t.Cleanup(func() { commsBackendFactory = old })
}

func newCommsFixture(t *testing.T) *testFixture {
	t.Helper()
	tf := &testFixture{
		t:      t,
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.EnvURL, "http://example.test")
	t.Setenv(config.EnvAuditLog, filepath.Join(home, "audit.log"))
	t.Setenv(config.EnvAgentName, "agent-1")
	t.Setenv("CONCEPT_CHAT_MM_URL", "https://mm.example.test")
	return tf
}

// =============================================================================
// arg validation
// =============================================================================

func TestCommsJoin_RequiresBackend(t *testing.T) {
	f := newCommsFixture(t)
	err := f.run(NewCommsJoinCmd())
	if err != nil {
		t.Fatalf("best-effort: should exit 0, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--backend is required") {
		t.Fatalf("stderr should require backend: %q", f.stderr.String())
	}
}

func TestCommsJoin_SlackIsStub(t *testing.T) {
	f := newCommsFixture(t)
	err := f.run(NewCommsJoinCmd(), "--backend", "slack")
	if err == nil {
		t.Fatal("slack stub should return an error")
	}
	if !strings.Contains(f.stderr.String(), "not yet implemented") {
		t.Fatalf("stderr should mark v0.4 stub: %q", f.stderr.String())
	}
}

func TestCommsJoin_DiscordIsStub(t *testing.T) {
	f := newCommsFixture(t)
	err := f.run(NewCommsJoinCmd(), "--backend", "discord")
	if err == nil {
		t.Fatal("discord stub should return an error")
	}
}

func TestCommsJoin_NoneIsNoop(t *testing.T) {
	f := newCommsFixture(t)
	err := f.run(NewCommsJoinCmd(), "--backend", "none")
	if err != nil {
		t.Fatalf("none should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "no-op") {
		t.Fatalf("stderr should announce no-op: %q", f.stderr.String())
	}
}

// =============================================================================
// happy path (mattermost)
// =============================================================================

func TestCommsJoin_Mattermost_HappyPath(t *testing.T) {
	f := newCommsFixture(t)
	fb := &fakeBackend{}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	err := f.run(NewCommsJoinCmd(),
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
		"--bot-name", "agent-1-bot",
		"--channel", "agent-comms",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if fb.validateCalls != 1 || fb.ensureCalls != 1 || fb.teamMemberCalls != 1 || fb.addToChCalls != 1 || fb.mintCalls != 1 {
		t.Fatalf("call counts: validate=%d ensure=%d team=%d add=%d mint=%d",
			fb.validateCalls, fb.ensureCalls, fb.teamMemberCalls, fb.addToChCalls, fb.mintCalls)
	}
	// PAT was written chmod 600.
	home := os.Getenv("HOME")
	patPath := filepath.Join(home, ".config", "concept-workflow", "mattermost-bot-pat")
	info, err := os.Stat(patPath)
	if err != nil {
		t.Fatalf("stat PAT: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("PAT must be chmod 600; got %o", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(patPath)
	if string(raw) != "fresh-pat-value" {
		t.Fatalf("PAT contents wrong: %q", string(raw))
	}
	// concept-chat.env was written with the three env keys.
	envPath := filepath.Join(home, ".config", "concept-workflow", "concept-chat.env")
	rawEnv, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	for _, expect := range []string{
		`export CONCEPT_CHAT_MM_URL="https://mm.example.test"`,
		`export CONCEPT_CHAT_MM_PAT_FILE=`,
		`export CONCEPT_CHAT_MM_CHANNEL="agent-comms"`,
	} {
		if !strings.Contains(string(rawEnv), expect) {
			t.Errorf("env file missing %q; body=\n%s", expect, string(rawEnv))
		}
	}
}

func TestCommsJoin_DefaultsBotName(t *testing.T) {
	f := newCommsFixture(t)
	fb := &fakeBackend{}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	err := f.run(NewCommsJoinCmd(),
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if fb.lastBotName != "agent-1-bot" {
		t.Fatalf("default bot-name should be <AGENT_NAME>-bot; got %q", fb.lastBotName)
	}
}

func TestCommsJoin_DefaultsChannel(t *testing.T) {
	f := newCommsFixture(t)
	fb := &fakeBackend{}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	err := f.run(NewCommsJoinCmd(),
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if fb.lastChannel != "agent-comms" {
		t.Fatalf("default channel should be agent-comms; got %q", fb.lastChannel)
	}
}

// =============================================================================
// idempotency
// =============================================================================

func TestCommsJoin_IdempotentRerun_SkipsMint(t *testing.T) {
	f := newCommsFixture(t)
	fb := &fakeBackend{}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	// Pre-seed an existing PAT file chmod 600.
	home := os.Getenv("HOME")
	cfgDir := filepath.Join(home, ".config", "concept-workflow")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	patPath := filepath.Join(cfgDir, "mattermost-bot-pat")
	if err := os.WriteFile(patPath, []byte("EXISTING-PAT"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := f.run(NewCommsJoinCmd(),
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if fb.mintCalls != 0 {
		t.Fatalf("expected no mint call; got %d", fb.mintCalls)
	}
	if !strings.Contains(f.stderr.String(), "PAT already present") {
		t.Fatalf("stderr should announce idempotent skip: %q", f.stderr.String())
	}
	// PAT contents preserved.
	raw, _ := os.ReadFile(patPath)
	if string(raw) != "EXISTING-PAT" {
		t.Fatalf("PAT should not be overwritten; got %q", string(raw))
	}
}

func TestCommsJoin_RotateForcesFreshMint(t *testing.T) {
	f := newCommsFixture(t)
	fb := &fakeBackend{}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	// Pre-seed an existing PAT file chmod 600.
	home := os.Getenv("HOME")
	cfgDir := filepath.Join(home, ".config", "concept-workflow")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	patPath := filepath.Join(cfgDir, "mattermost-bot-pat")
	if err := os.WriteFile(patPath, []byte("EXISTING-PAT"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := f.run(NewCommsJoinCmd(),
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
		"--rotate",
	)
	if err != nil {
		t.Fatalf("run rotate: %v stderr=%s", err, f.stderr.String())
	}
	if fb.mintCalls != 1 {
		t.Fatalf("expected 1 mint call with --rotate; got %d", fb.mintCalls)
	}
	raw, _ := os.ReadFile(patPath)
	if string(raw) != "fresh-pat-value" {
		t.Fatalf("PAT should be rotated; got %q", string(raw))
	}
}

// =============================================================================
// error pass-through
// =============================================================================

func TestCommsJoin_ValidateError_BestEffort(t *testing.T) {
	f := newCommsFixture(t)
	fb := &fakeBackend{validateErr: fmt.Errorf("HTTP 401")}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	err := f.run(NewCommsJoinCmd(),
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
	)
	if err != nil {
		t.Fatalf("best-effort: should exit 0, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "validate admin PAT") {
		t.Fatalf("stderr should mention the failing step: %q", f.stderr.String())
	}
}

func TestCommsJoin_ValidateError_Strict(t *testing.T) {
	f := newCommsFixture(t)
	fb := &fakeBackend{validateErr: fmt.Errorf("HTTP 401")}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	err := f.run(NewCommsJoinCmd(), "--strict",
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
	)
	if err == nil {
		t.Fatal("strict mode should propagate")
	}
}

// =============================================================================
// bug #49 — team-add must precede channel-add
// =============================================================================

func TestCommsJoin_EnsureTeamMemberBeforeChannelAdd(t *testing.T) {
	// Bug #49: on a fresh Mattermost team the bot account is not yet a team
	// member, so AddBotToChannel returns 403 user_not_in_team. comms-join
	// must call EnsureTeamMember between EnsureBotUser and AddBotToChannel.
	f := newCommsFixture(t)
	fb := &fakeBackend{}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	err := f.run(NewCommsJoinCmd(),
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
	)
	if err != nil {
		t.Fatalf("run: %v stderr=%s", err, f.stderr.String())
	}
	if fb.teamMemberCalls != 1 {
		t.Fatalf("EnsureTeamMember should be called exactly once; got %d", fb.teamMemberCalls)
	}
	// Locate the relative ordering inside callOrder.
	idxEnsureBot, idxTeamMember, idxAddCh := -1, -1, -1
	for i, c := range fb.callOrder {
		switch c {
		case "ensure-bot":
			idxEnsureBot = i
		case "ensure-team-member":
			idxTeamMember = i
		case "add-to-channel":
			idxAddCh = i
		}
	}
	if idxEnsureBot < 0 || idxTeamMember < 0 || idxAddCh < 0 {
		t.Fatalf("call order missing entries: %v", fb.callOrder)
	}
	if !(idxEnsureBot < idxTeamMember && idxTeamMember < idxAddCh) {
		t.Fatalf(
			"call order must be ensure-bot → ensure-team-member → add-to-channel; got %v",
			fb.callOrder,
		)
	}
}

func TestCommsJoin_TeamMemberError_BestEffort(t *testing.T) {
	f := newCommsFixture(t)
	fb := &fakeBackend{teamMemberErr: fmt.Errorf("HTTP 403")}
	withFakeBackend(t, fb)
	t.Setenv("MM_ADMIN_TOK", "admin-bearer")

	err := f.run(NewCommsJoinCmd(),
		"--backend", "mattermost",
		"--bootstrap-pat", "env:MM_ADMIN_TOK",
	)
	if err != nil {
		t.Fatalf("best-effort: should exit 0, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "ensure team member") {
		t.Fatalf("stderr should mention failing step: %q", f.stderr.String())
	}
	// And AddBotToChannel must NOT have been called (we halt at team-add).
	if fb.addToChCalls != 0 {
		t.Fatalf("AddBotToChannel should be skipped after team-add fails; got %d calls", fb.addToChCalls)
	}
}
