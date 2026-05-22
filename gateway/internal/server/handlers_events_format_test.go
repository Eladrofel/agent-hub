package server

import (
	"strings"
	"testing"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
)

// =============================================================================
// v0.1.9 — curated message formatter (agent.improvement-note)
//
// formatCuratedMessage returns "" for every event_type EXCEPT
// agent.improvement-note (where it produces the "💡 <alias>: <summary>"
// one-liner). The empty-string contract lets events.InsertWithOutbox fall
// back to its default "[event_type] summary" composer for everything else.
// =============================================================================

func TestFormatCuratedMessage_NonImprovementReturnsEmpty(t *testing.T) {
	agent := &auth.Agent{Name: "agent-1", Alias: "Splinter"}
	for _, et := range []string{
		"task.created", "task.completed", "session.ended",
		"sanitiser.blocked", "decision.proposed", "anything.else",
	} {
		got := formatCuratedMessage(et, agent, "doesn't matter", nil)
		if got != "" {
			t.Errorf("event_type=%q: want \"\", got %q", et, got)
		}
	}
}

func TestFormatImprovementNote_AliasPreferred(t *testing.T) {
	agent := &auth.Agent{Name: "agent-operator-mac", Alias: "Splinter"}
	got := formatCuratedMessage("agent.improvement-note", agent,
		"the bot-to-bot relay needs a config toggle", nil)
	want := "\xf0\x9f\x92\xa1 Splinter: the bot-to-bot relay needs a config toggle"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if !strings.HasPrefix(got, "\xf0\x9f\x92\xa1 ") {
		t.Errorf("missing leading lightbulb emoji; got %q", got)
	}
}

func TestFormatImprovementNote_AliasFallsBackToName(t *testing.T) {
	agent := &auth.Agent{Name: "agent-vm-3", Alias: ""}
	got := formatCuratedMessage("agent.improvement-note", agent,
		"falls back to canonical name", nil)
	if !strings.Contains(got, "agent-vm-3:") {
		t.Fatalf("alias-empty should fall back to Name; got %q", got)
	}
}

func TestFormatImprovementNote_ContextAppendedItalicised(t *testing.T) {
	agent := &auth.Agent{Name: "agent-1", Alias: "Mikey"}
	payload := map[string]any{
		"context": "feat-04-bulk-import",
	}
	got := formatCuratedMessage("agent.improvement-note", agent,
		"observed surprising fixture shape", payload)
	want := "\xf0\x9f\x92\xa1 Mikey: observed surprising fixture shape _(feat-04-bulk-import)_"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestFormatImprovementNote_EmptyContextOmitted(t *testing.T) {
	agent := &auth.Agent{Name: "agent-1", Alias: "Donnie"}
	payload := map[string]any{
		"context": "   ", // whitespace-only treated as empty
	}
	got := formatCuratedMessage("agent.improvement-note", agent,
		"context whitespace gets trimmed off", payload)
	if strings.Contains(got, "_(") {
		t.Fatalf("whitespace-only context should not produce italic block; got %q", got)
	}
}

func TestFormatImprovementNote_DetailsNotInlinedV019(t *testing.T) {
	// v0.1.9 keeps the MM line short; details live on the durable row only.
	// If a future release changes this, the test should be updated alongside
	// the formatter (and the agentctl side / CHANGELOG).
	agent := &auth.Agent{Name: "agent-1", Alias: "Raph"}
	payload := map[string]any{
		"details": "a much longer body that should NOT appear in the chat post",
	}
	got := formatCuratedMessage("agent.improvement-note", agent,
		"short summary only", payload)
	if strings.Contains(got, "much longer body") {
		t.Fatalf("details should NOT be inlined in v0.1.9; got %q", got)
	}
}

func TestFormatImprovementNote_NilAgentSafeFallback(t *testing.T) {
	// Defensive: the handler should never call us with a nil agent, but if
	// the contract ever changes we don't want a nil-deref panic.
	got := formatCuratedMessage("agent.improvement-note", nil, "x", nil)
	if !strings.Contains(got, "agent:") {
		t.Fatalf("nil agent should fall back to 'agent'; got %q", got)
	}
}

func TestFormatImprovementNote_SummaryTrimmed(t *testing.T) {
	agent := &auth.Agent{Name: "agent-1", Alias: "Splinter"}
	got := formatCuratedMessage("agent.improvement-note", agent,
		"   leading + trailing whitespace   ", nil)
	if strings.Contains(got, ":  ") || strings.HasSuffix(got, "   ") {
		t.Fatalf("summary should be trimmed; got %q", got)
	}
}
