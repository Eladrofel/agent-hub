package server

// Pure-unit tests for lifecycle-event summary formatters (v0.1.8). The
// formatters take an auth.Agent + a few request fields and produce the
// operator-facing summary string that the curated-event outbox surfaces
// in Mattermost. No DB / no HTTP / no router — table-driven.
//
// Bug being guarded against: v0.1.7 emitted bare summaries like
// "session ended for agent-operator-mac" with no session id, no
// final_status, no project. Operator couldn't correlate the chat ping
// with the underlying session without re-querying.

import (
	"strings"
	"testing"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
)

func TestFormatStartSummary_AliasAndSessionAndProject(t *testing.T) {
	got := formatStartSummary(
		&auth.Agent{Name: "agent-operator-mac", Alias: "Splinter"},
		"12345678-aaaa-bbbb-cccc-deadbeefcafe",
		"demo",
	)
	want := "[start] Splinter — session 12345678, project=demo"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFormatStartSummary_NoProject(t *testing.T) {
	got := formatStartSummary(
		&auth.Agent{Name: "agent-3", Alias: "Mikey"},
		"abcdef0123456789",
		"",
	)
	want := "[start] Mikey — session abcdef01"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFormatStartSummary_FallsBackToCanonicalNameWhenNoAlias(t *testing.T) {
	got := formatStartSummary(
		&auth.Agent{Name: "agent-7"}, // no alias
		"12345678-xxxx",
		"web",
	)
	if !strings.HasPrefix(got, "[start] agent-7 — session 12345678") {
		t.Errorf("expected prefix '[start] agent-7 — session 12345678', got %q", got)
	}
}

func TestFormatEndSummary_WithFinalStatusClear(t *testing.T) {
	got := formatEndSummary(
		&auth.Agent{Name: "agent-operator-mac", Alias: "Splinter"},
		"12345678-aaaa-bbbb-cccc-deadbeefcafe",
		"clear",
	)
	want := "[end] Splinter — session 12345678, clear"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFormatEndSummary_WithFinalStatusUserExit(t *testing.T) {
	got := formatEndSummary(
		&auth.Agent{Name: "agent-operator-mac", Alias: "Splinter"},
		"12345678-aaaa-bbbb-cccc-deadbeefcafe",
		"user_exit",
	)
	want := "[end] Splinter — session 12345678, user_exit"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFormatEndSummary_OmitsFinalStatusWhenEmpty(t *testing.T) {
	got := formatEndSummary(
		&auth.Agent{Name: "agent-3", Alias: "Donnie"},
		"feedface00000000",
		"",
	)
	want := "[end] Donnie — session feedface"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFormatCheckpointSummary_PrefersUserSummary(t *testing.T) {
	got := formatCheckpointSummary(
		&auth.Agent{Name: "agent-2", Alias: "Raph"},
		"12345678-xxxx",
		"refactored handler, all tests green",
		"in_progress",
	)
	want := "[checkpoint] Raph — session 12345678 — refactored handler, all tests green"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFormatCheckpointSummary_FallsBackToStatusWhenSummaryEmpty(t *testing.T) {
	got := formatCheckpointSummary(
		&auth.Agent{Name: "agent-2", Alias: "Raph"},
		"12345678-xxxx",
		"",
		"blocked",
	)
	want := "[checkpoint] Raph — session 12345678, status=blocked"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestShortSessionID_HandlesShortStrings(t *testing.T) {
	// Defensive: claude_session_id is validated as non-empty at the
	// handler layer, but the formatter shouldn't slice-panic on anything
	// shorter than 8 chars.
	cases := map[string]string{
		"":         "",
		"abc":      "abc",
		"12345678": "12345678",
		"123456789": "12345678",
	}
	for in, want := range cases {
		if got := shortSessionID(in); got != want {
			t.Errorf("shortSessionID(%q) = %q, want %q", in, got, want)
		}
	}
}
