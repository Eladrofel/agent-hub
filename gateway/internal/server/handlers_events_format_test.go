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

// v0.1.14 — work-item events get their own one-liners:
//
//	🔵 <alias>: <summary>   (claimed)
//	✅ <alias>: <summary>   (finished)
//
// The summary is composed agentctl-side and already carries the wi-key,
// repo, [forced] suffix, and PR URL — the formatter just leads with an icon
// + alias so chat scanning works the same way it does for improvement-notes.

func TestFormatCuratedMessage_WorkItemClaimedHasBlueCircle(t *testing.T) {
	agent := &auth.Agent{Name: "agent-1", Alias: "Mikey"}
	got := formatCuratedMessage("agent.work-item.claimed", agent,
		"claimed feat-04-bulk-import (customer-web)", nil)
	want := "\U0001f535 Mikey: claimed feat-04-bulk-import (customer-web)"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestFormatCuratedMessage_WorkItemFinishedHasCheckMark(t *testing.T) {
	agent := &auth.Agent{Name: "agent-2", Alias: "Donnie"}
	got := formatCuratedMessage("agent.work-item.finished", agent,
		"finished feat-04-bulk-import (customer-web) — https://forge/pr/42", nil)
	want := "✅ Donnie: finished feat-04-bulk-import (customer-web) — https://forge/pr/42"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestFormatCuratedMessage_WorkItemAliasFallsBackToName(t *testing.T) {
	agent := &auth.Agent{Name: "agent-3", Alias: ""}
	got := formatCuratedMessage("agent.work-item.claimed", agent,
		"claimed feat-99 (customer-web)", nil)
	if !strings.Contains(got, "agent-3:") {
		t.Fatalf("alias-empty should fall back to Name; got %q", got)
	}
}

func TestFormatCuratedMessage_WorkItemForcedSuffixPreserved(t *testing.T) {
	agent := &auth.Agent{Name: "agent-2", Alias: "Donnie"}
	got := formatCuratedMessage("agent.work-item.claimed", agent,
		"claimed feat-04-bulk-import (customer-web) [forced]", nil)
	if !strings.HasSuffix(got, "[forced]") {
		t.Fatalf("force suffix should pass through verbatim; got %q", got)
	}
}

// v0.1.18 — peer-message format. Shape:
//
//	@<target> <intent-icon> <sender>: <summary>
//
// The leading @<target> is load-bearing for the MM outgoing-webhook routing
// path. Icon comes from payload.intent (default 💬 for info).

func TestFormatCuratedMessage_PeerMessageDefaultsToSpeechBalloon(t *testing.T) {
	agent := &auth.Agent{Name: "agent-1", Alias: "Mikey"}
	got := formatCuratedMessage("agent.peer-message", agent,
		"suggestions on task-04: consider memoising the row formatter",
		map[string]any{"target_agent": "Donnie"})
	want := "@Donnie \U0001f4ac Mikey: suggestions on task-04: consider memoising the row formatter"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestFormatCuratedMessage_PeerMessageQuestionUsesQuestionMark(t *testing.T) {
	agent := &auth.Agent{Name: "agent-1", Alias: "Mikey"}
	got := formatCuratedMessage("agent.peer-message", agent,
		"are you blocked on task-04?",
		map[string]any{"target_agent": "Donnie", "intent": "question"})
	if !strings.HasPrefix(got, "@Donnie ❓ Mikey:") {
		t.Fatalf("question intent should render ❓; got %q", got)
	}
}

func TestFormatCuratedMessage_PeerMessageBlockerUsesNoEntrySign(t *testing.T) {
	agent := &auth.Agent{Name: "agent-2", Alias: "Donnie"}
	got := formatCuratedMessage("agent.peer-message", agent,
		"task-04 is blocked: forge auth missing",
		map[string]any{"target_agent": "Splinter", "intent": "blocker"})
	if !strings.HasPrefix(got, "@Splinter \U0001f6ab Donnie:") {
		t.Fatalf("blocker intent should render 🚫; got %q", got)
	}
}

func TestFormatCuratedMessage_PeerMessageNoTargetOmitsAtPrefix(t *testing.T) {
	// Edge case: target_agent missing. Don't fabricate an @-prefix; render
	// the rest of the message normally. Caller can still see it in MM, just
	// won't trigger inbox routing.
	agent := &auth.Agent{Name: "agent-1", Alias: "Mikey"}
	got := formatCuratedMessage("agent.peer-message", agent,
		"orphan message", nil)
	if strings.HasPrefix(got, "@") {
		t.Fatalf("no target_agent → no leading @; got %q", got)
	}
	if !strings.HasPrefix(got, "\U0001f4ac Mikey:") {
		t.Fatalf("should still render icon+sender+summary; got %q", got)
	}
}

func TestFormatCuratedMessage_PeerMessageAliasFallsBackToName(t *testing.T) {
	agent := &auth.Agent{Name: "agent-3", Alias: ""}
	got := formatCuratedMessage("agent.peer-message", agent,
		"hello", map[string]any{"target_agent": "Splinter"})
	if !strings.Contains(got, "agent-3:") {
		t.Fatalf("alias-empty should fall back to Name; got %q", got)
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
