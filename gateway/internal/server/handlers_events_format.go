package server

import (
	"fmt"
	"strings"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
)

// =============================================================================
// Per-event-type curated message formatters (v0.1.9)
//
// The outbox-worker is dumb: it takes the `message` text on the mattermost_outbox
// row and posts it verbatim. Per-event-type rendering therefore lives at the
// enqueue side. For most curated event_types we hand events.InsertWithOutbox an
// empty `message` and let it compose the default "[event_type] summary" shape.
//
// v0.1.9 adds agent.improvement-note, which gets a distinct one-liner:
//
//	💡 <alias>: <summary> _(<context>)_
//
// The leading emoji is the visual differentiator from the lifecycle stream
// ([start]/[checkpoint]/[end]) — operators triaging an active channel can pick
// learnings out of routine session traffic at a glance. Alias falls back to
// canonical agent name when unset (same rule as lifecycle summaries — see
// handlers_sessions.go::callerDisplayName).
//
// The sanitiser already ran on Summary + Payload BEFORE this formatter is
// called (handleEventEmit gates on it). The formatter does not bypass the
// sanitiser; it only renders the already-cleared fields into a chat-friendly
// shape.
// =============================================================================

// formatCuratedMessage returns the message body that will be enqueued for the
// outbox-worker to post. Returning "" means "let events.InsertWithOutbox
// compose the default" — that's the behaviour for every curated event_type
// the gateway shipped before v0.1.9 plus any future curated type that doesn't
// register a dedicated formatter here.
func formatCuratedMessage(eventType string, agent *auth.Agent, summary string, payload map[string]any) string {
	switch eventType {
	case "agent.improvement-note":
		return formatImprovementNote(agent, summary, payload)
	case "agent.work-item.claimed":
		return formatWorkItem("\U0001f535", agent, summary) // 🔵
	case "agent.work-item.finished":
		return formatWorkItem("✅", agent, summary) // ✅
	}
	return ""
}

// formatWorkItem renders the chat-side line for an agent.work-item.{claimed,
// finished} event. Shape:
//
//	🔵 <alias>: claimed <wi-key> (<repo>) [forced]
//	✅ <alias>: finished <wi-key> (<repo>) — <pr-url>
//
// The summary text (composed agentctl-side in commands/work_item.go) already
// carries the wi-key, repo, [forced] suffix, and PR URL. We just lead with
// an icon + alias for chat scan-ability — same treatment as improvement-note.
func formatWorkItem(icon string, agent *auth.Agent, summary string) string {
	alias := "agent"
	if agent != nil {
		alias = callerDisplayName(agent)
	}
	return fmt.Sprintf("%s %s: %s", icon, alias, strings.TrimSpace(summary))
}

// formatImprovementNote renders the chat-side body for an agent.improvement-note
// event. Shape:
//
//	💡 <alias>: <summary> _(<context>)_
//
// Rules (locked with the agentctl side):
//   - alias falls back to canonical name when alias is empty
//   - summary is taken from the top-level event summary (gateway already
//     enforced its sanitiser pass + the 280-char cap on the agentctl side)
//   - context (payload.context) is appended in italics inside parens when set
//   - details (payload.details) is intentionally NOT inlined for v0.1.9 —
//     propagation_hint=mm keeps the chat line short; details still lives on
//     the durable event row for query.
func formatImprovementNote(agent *auth.Agent, summary string, payload map[string]any) string {
	alias := "agent"
	if agent != nil {
		alias = callerDisplayName(agent)
	}
	body := fmt.Sprintf("💡 %s: %s", alias, strings.TrimSpace(summary))
	if payload != nil {
		if ctx, ok := payload["context"].(string); ok {
			ctx = strings.TrimSpace(ctx)
			if ctx != "" {
				body = body + fmt.Sprintf(" _(%s)_", ctx)
			}
		}
	}
	return body
}
