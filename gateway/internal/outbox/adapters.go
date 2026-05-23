// Package outbox — message adapters (v0.1.10).
//
// MessageAdapter is the pluggable interface that takes a curated event and
// renders it into the per-backend shape (Mattermost message attachments,
// Slack legacy attachments, Discord embeds). The gateway's POST /v1/events
// enqueue path picks an adapter, calls FormatEvent, and stores the result
// in the outbox row's props field. The outbox-worker is dumb: it forwards
// props.attachments to the Mattermost API.
//
// Slack + Discord adapters live here for future activation (different
// backends, same enqueue path). v0.1.10 only ships Mattermost as the live
// path; the others are exercised in unit tests but not wired into the
// runtime selector — that's a v0.1.11+ change.
//
// Backend selection is a runtime choice; today the outbox-worker is
// hard-coded against Mattermost via MATTERMOST_URL / MATTERMOST_TOKEN
// env vars (see worker.go::DefaultWorkerConfig). v0.1.10 keeps that path
// unchanged: Mattermost is the default backend, Slack + Discord adapters
// are inert until the worker is taught to pick between them.
//
// Color + icon resolution rules are documented in FormatterInputs.
package outbox

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatterInputs is the per-event input passed to every adapter. The
// caller (gateway events handler) is responsible for resolving the project
// slug, the agent alias, and pulling intent out of the payload before
// calling Format. This keeps the adapter pure (no DB calls).
//
// Intent is the v0.1.10 enum (info|directive|question|blocker|status); "" =
// absent (treated as info for color / icon precedence).
type FormatterInputs struct {
	EventType   string
	Alias       string         // agent display name (alias falls back to name upstream)
	ProjectSlug string         // empty if unresolvable
	Summary     string         // event summary (already sanitiser-checked)
	Intent      string         // v0.1.10 — info|directive|question|blocker|status, empty = info
	Payload     map[string]any // raw event payload (for adapter-specific field extraction)
}

// MessageAdapter renders a curated event into a backend-specific message
// payload suitable for storing in outbox row props. The returned map is
// merged into props verbatim by the caller; for Mattermost / Slack the
// shape is {"attachments": [...]}, for Discord it is {"embeds": [...]}.
type MessageAdapter interface {
	FormatEvent(in FormatterInputs) (map[string]any, error)
	Backend() string
}

// =============================================================================
// Color + icon precedence (shared by Mattermost / Slack; Discord overrides
// the color encoding to decimal int).
// =============================================================================

// intentColor returns the v0.1.10 intent-based color (hex), or "" if intent
// is empty / unknown.
func intentColor(intent string) string {
	switch intent {
	case "info":
		return "#0d6efd" // blue
	case "directive":
		return "#fd7e14" // orange — operator authority
	case "question":
		return "#ffc107" // yellow
	case "blocker":
		return "#dc3545" // red
	case "status":
		return "#6c757d" // gray
	}
	return ""
}

// eventTypeColor returns the fallback color used when intent is unset. The
// curated event_types that ship pre-v0.1.10 each get a fixed color; unknown
// curated types default to neutral gray.
func eventTypeColor(eventType string) string {
	switch eventType {
	case "session.started":
		return "#20c997" // teal
	case "session.ended", "session.checkpointed", "agent.improvement-note":
		return "#6f42c1" // purple
	}
	return "#6c757d" // gray
}

// resolveColor applies the v0.1.10 precedence: intent wins, event_type
// fallback otherwise.
func resolveColor(eventType, intent string) string {
	if c := intentColor(intent); c != "" {
		return c
	}
	return eventTypeColor(eventType)
}

// intentIcon returns the v0.1.10 intent-based unicode icon, or "" if intent
// is empty / unknown.
func intentIcon(intent string) string {
	switch intent {
	case "info":
		return "ℹ️" // information source
	case "directive":
		return "⚡" // high voltage
	case "question":
		return "❓" // red question mark
	case "blocker":
		return "\U0001f6ab" // no entry sign
	case "status":
		return "\U0001f4ca" // bar chart
	}
	return ""
}

// eventTypeIcon returns the fallback icon for event_type when intent is
// unset. Pre-v0.1.10 lifecycle types keep their familiar markers;
// agent.improvement-note keeps the v0.1.9 lightbulb regardless of intent
// (the lightbulb is the icon operators already recognise — preserving it
// avoids regression in chat scan-ability).
func eventTypeIcon(eventType string) string {
	switch eventType {
	case "session.started":
		return "\U0001f7e2" // green circle
	case "session.ended":
		return "\U0001f534" // red circle
	case "session.checkpointed":
		return "\U0001f4cd" // round pushpin
	}
	return ""
}

// resolveIcon applies the v0.1.10 precedence:
//   - agent.improvement-note ALWAYS uses lightbulb (v0.1.9 preserve)
//   - else intent icon if intent is set
//   - else event_type icon
//   - else empty string
func resolveIcon(eventType, intent string) string {
	if eventType == "agent.improvement-note" {
		return "\U0001f4a1" // light bulb
	}
	if i := intentIcon(intent); i != "" {
		return i
	}
	return eventTypeIcon(eventType)
}

// composeTitle builds "<icon> <alias> @ <project_slug>" (or "<icon> <alias>"
// when no project slug is resolvable). Icon-leading-space is dropped when
// the icon is empty so the title doesn't start with whitespace.
func composeTitle(icon, alias, projectSlug string) string {
	parts := []string{}
	if icon != "" {
		parts = append(parts, icon)
	}
	if alias == "" {
		alias = "agent"
	}
	if projectSlug != "" {
		parts = append(parts, fmt.Sprintf("%s @ %s", alias, projectSlug))
	} else {
		parts = append(parts, alias)
	}
	return strings.Join(parts, " ")
}

// composeFallback is the plain-text body for clients that don't render
// attachments (notifications, mobile push). Mirrors v0.1.9 single-line
// shape so the old format still reads naturally.
func composeFallback(icon, alias, summary string) string {
	if alias == "" {
		alias = "agent"
	}
	if icon == "" {
		return fmt.Sprintf("%s: %s", alias, summary)
	}
	return fmt.Sprintf("%s %s: %s", icon, alias, summary)
}

// extraFields produces the per-event-type structured fields. v0.1.10 only
// populates for agent.improvement-note (Category + Context, when set);
// other event_types return an empty slice. Extensible — new curated types
// can hook in here without touching adapter code.
func extraFields(eventType string, payload map[string]any) []map[string]any {
	if eventType != "agent.improvement-note" || payload == nil {
		return nil
	}
	var fields []map[string]any
	if cat, ok := payload["category"].(string); ok && cat != "" {
		fields = append(fields, map[string]any{
			"title": "Category",
			"value": cat,
			"short": true,
		})
	}
	if ctx, ok := payload["context"].(string); ok {
		if s := strings.TrimSpace(ctx); s != "" {
			fields = append(fields, map[string]any{
				"title": "Context",
				"value": s,
				"short": true,
			})
		}
	}
	return fields
}

// =============================================================================
// MattermostAdapter — produces props.attachments matching the Mattermost
// /api/v4/posts message-attachments API. Slack's legacy attachments API is
// near-identical; SlackAdapter reuses this shape.
// =============================================================================

type MattermostAdapter struct{}

func (MattermostAdapter) Backend() string { return "mattermost" }

func (MattermostAdapter) FormatEvent(in FormatterInputs) (map[string]any, error) {
	icon := resolveIcon(in.EventType, in.Intent)
	att := map[string]any{
		"color":    resolveColor(in.EventType, in.Intent),
		"fallback": composeFallback(icon, in.Alias, in.Summary),
		"title":    composeTitle(icon, in.Alias, in.ProjectSlug),
		"text":     in.Summary,
	}
	if fields := extraFields(in.EventType, in.Payload); len(fields) > 0 {
		att["fields"] = fields
	}
	return map[string]any{
		"attachments": []map[string]any{att},
	}, nil
}

// =============================================================================
// SlackAdapter — Slack's legacy attachments API uses the same color/title/
// fallback/text/fields shape as Mattermost. Direct adaptation; the only
// difference is the wrapper field name ("attachments" — same).
// =============================================================================

type SlackAdapter struct{}

func (SlackAdapter) Backend() string { return "slack" }

func (SlackAdapter) FormatEvent(in FormatterInputs) (map[string]any, error) {
	// Same shape as Mattermost. Kept as a distinct type to allow per-
	// backend divergence later (Slack's blocks API, threading hints,
	// etc.) without touching the MM path.
	return MattermostAdapter{}.FormatEvent(in)
}

// =============================================================================
// DiscordAdapter — Discord embeds use a different field naming:
//   - "color" is a decimal integer (not a hex string)
//   - "title" + "description" (not text)
//   - "fields" still uses {name, value, inline} (not title/value/short)
//   - wrapper is "embeds", an array of embed objects
// =============================================================================

type DiscordAdapter struct{}

func (DiscordAdapter) Backend() string { return "discord" }

func (DiscordAdapter) FormatEvent(in FormatterInputs) (map[string]any, error) {
	icon := resolveIcon(in.EventType, in.Intent)
	hex := resolveColor(in.EventType, in.Intent)
	color, err := hexToDecimal(hex)
	if err != nil {
		return nil, fmt.Errorf("discord adapter: %w", err)
	}
	embed := map[string]any{
		"color":       color,
		"title":       composeTitle(icon, in.Alias, in.ProjectSlug),
		"description": in.Summary,
	}
	if fields := extraFields(in.EventType, in.Payload); len(fields) > 0 {
		discord := make([]map[string]any, 0, len(fields))
		for _, f := range fields {
			discord = append(discord, map[string]any{
				"name":   f["title"],
				"value":  f["value"],
				"inline": f["short"],
			})
		}
		embed["fields"] = discord
	}
	return map[string]any{
		"embeds": []map[string]any{embed},
	}, nil
}

// hexToDecimal converts "#rrggbb" → 0xrrggbb decimal. Empty hex defaults
// to 0 (Discord renders this as the default grey accent).
func hexToDecimal(hex string) (int, error) {
	hex = strings.TrimPrefix(hex, "#")
	if hex == "" {
		return 0, nil
	}
	if len(hex) != 6 {
		return 0, fmt.Errorf("invalid hex color %q (want #rrggbb)", hex)
	}
	n, err := strconv.ParseInt(hex, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("parse hex %q: %w", hex, err)
	}
	return int(n), nil
}

// =============================================================================
// Backend selector — picks an adapter by name. Used by the worker / future
// per-project backend config. Defaults to mattermost.
// =============================================================================

// AdapterFor returns the adapter for the named backend. Unknown names fall
// back to Mattermost — that's the v0.1.10 default and the only backend
// actually wired up at the worker level.
func AdapterFor(backend string) MessageAdapter {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "slack":
		return SlackAdapter{}
	case "discord":
		return DiscordAdapter{}
	default:
		return MattermostAdapter{}
	}
}
