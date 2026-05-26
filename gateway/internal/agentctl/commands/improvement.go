// improvement.go — `agentctl improvement emit` subcommand (v0.1.9).
//
// Captured learnings during work — what was figured out, what surprised the
// agent, what should change — are first-class data. They belong on the event
// bus (durable, queryable) and, when worth surfacing, on Mattermost (curated,
// visible). They explicitly do NOT belong in source control. The shape of an
// improvement-note is locked across the agentctl + plugin sides:
//
//	{
//	  "category":          "process",     // enum, see ImprovementCategories
//	  "summary":           "...",         // required, ≤ 280 chars (MM-post friendly)
//	  "context":           "feat-04-x",   // optional, e.g. work-item key
//	  "propagation_hint":  "mm",          // none|mm|fleet  (fleet treated as mm in v0.1.9)
//	  "details":           "..."          // optional longer body (no cap)
//	}
//
// This subcommand does NOT duplicate the wire / auth / strict-mode plumbing
// from `agentctl event emit` — it constructs the payload above and routes it
// through the existing /v1/events POST path. The gateway's per-event-type
// formatter (handlers_events_format.go) renders the MM-side line as
// `💡 <alias>: <summary>`.
package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// improvementNoteEventType is the curated event_type the gateway recognises
// (see events.CuratedEventTypes). Kept here as a constant so any future
// rename can be done in one place on the agentctl side too.
const improvementNoteEventType = "agent.improvement-note"

// improvementSummaryMaxRunes is the MM-post-friendly cap on the summary
// field. The gateway does NOT enforce this (the events table accepts any
// length); the CLI enforces it so the chat line stays one-screen-readable.
// Counted in runes, not bytes, so multi-byte UTF-8 doesn't truncate emoji.
const improvementSummaryMaxRunes = 280

// ImprovementCategories is the locked enum for --category. Adding a new
// category is a v0.2.x-class change because the plugin's /note-improvement
// skill and any downstream dashboards depend on the closed set.
var ImprovementCategories = []string{"architectural", "process", "tooling", "domain", "other"}

// ImprovementPropagation is the locked enum for --propagation:
//   - none:   durable event only (no MM relay even though event_type is curated;
//             the gateway always writes the outbox row for curated types, but
//             the agent's intent is recorded in propagation_hint so the
//             outbox-worker / future filters can honour it)
//   - mm:     curated relay to Mattermost (default behaviour for curated)
//   - fleet:  reserved for fleet-wide DM in a later release; treated as "mm"
//             in v0.1.9
var ImprovementPropagation = []string{"none", "mm", "fleet"}

// NewImprovementCmd is the `improvement` group; today it has only one
// subcommand. Wired into the root in cmd/agentctl/main.go.
func NewImprovementCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "improvement", Short: "Improvement-note subcommands"}
	cmd.AddCommand(newImprovementEmitCmd())
	return cmd
}

// newImprovementEmitCmd wraps POST /v1/events with event_type=agent.improvement-note.
// Goes through the same runCall / auditor / strict-mode pipeline as
// `agentctl event emit`.
func newImprovementEmitCmd() *cobra.Command {
	var (
		category        string
		summary         string
		contextStr      string
		propagation     string
		details         string
		intent          string
		claudeSessionID string
	)

	cmd := &cobra.Command{
		Use:   "emit",
		Short: "Emit one improvement-note (durable + optional Mattermost relay)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			auditor := audit.New(cfg.AuditLog)

			// Default propagation. Done before validation so an unset flag
			// passes the enum check.
			if propagation == "" {
				propagation = "none"
			}

			// ----------------------------------------------------------------
			// Arg validation (each failure routes through validationError so
			// best-effort vs. strict posture matches every other subcommand).
			// ----------------------------------------------------------------
			if category == "" {
				err := validationError(cmd, auditor, "improvement emit", fmt.Errorf("--category is required (one of %s)", strings.Join(ImprovementCategories, ", ")))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if !containsString(ImprovementCategories, category) {
				err := validationError(cmd, auditor, "improvement emit", fmt.Errorf("--category=%q invalid; must be one of %s", category, strings.Join(ImprovementCategories, ", ")))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if !containsString(ImprovementPropagation, propagation) {
				err := validationError(cmd, auditor, "improvement emit", fmt.Errorf("--propagation=%q invalid; must be one of %s", propagation, strings.Join(ImprovementPropagation, ", ")))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if err := validateIntent(intent); err != nil {
				err := validationError(cmd, auditor, "improvement emit", err)
				if IsSilent(err) {
					return nil
				}
				return err
			}
			summary = strings.TrimSpace(summary)
			if summary == "" {
				err := validationError(cmd, auditor, "improvement emit", fmt.Errorf("--summary is required (non-empty, ≤ %d chars)", improvementSummaryMaxRunes))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if n := utf8.RuneCountInString(summary); n > improvementSummaryMaxRunes {
				err := validationError(cmd, auditor, "improvement emit", fmt.Errorf("--summary is %d chars; max is %d (use --details for longer body)", n, improvementSummaryMaxRunes))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			// --details supports @file the same way curl does: resolve the
			// path BEFORE composing the payload so any IO error fails fast.
			if strings.HasPrefix(details, "@") {
				path := details[1:]
				raw, rerr := os.ReadFile(path)
				if rerr != nil {
					err := validationError(cmd, auditor, "improvement emit", fmt.Errorf("--details @%s: %w", path, rerr))
					if IsSilent(err) {
						return nil
					}
					return err
				}
				details = string(raw)
			}

			// ----------------------------------------------------------------
			// Compose payload + request body. The gateway sees this as a
			// vanilla POST /v1/events; the only thing that makes it special
			// is the event_type (curated → outbox relay) and the
			// per-event-type formatter on the server side.
			// ----------------------------------------------------------------
			payload := map[string]any{
				"category":         category,
				"summary":          summary,
				"propagation_hint": propagation,
			}
			if contextStr != "" {
				payload["context"] = contextStr
			}
			if details != "" {
				payload["details"] = details
			}
			// v0.1.10: --intent flag threads into the payload.intent field.
			// Absent / empty omits the field so the gateway's "absent → info"
			// contract holds. The gateway-side role check enforces
			// directive-only-from-operator regardless of event_type.
			if intent != "" {
				payload["intent"] = intent
			}

			body := map[string]any{
				"event_type": improvementNoteEventType,
				"summary":    summary, // top-level too so list queries don't need a JSONB deref
				"payload":    payload,
			}
			if cfg.ProjectSlug != "" {
				body["project_slug"] = cfg.ProjectSlug
			}

			// v0.1.11: improvement-notes MUST carry claude_session_id so the
			// gateway can resolve agent_session_id on insert. Without it the
			// row lands with agent_session_id=NULL and resume-context's per-
			// session event filter drops it → cross-/clear loses the captured
			// learning. Flag wins over env (the same precedence agentctl
			// checkpoint + resume-context use). Empty resolution is
			// best-effort: warn-to-stderr but don't halt — one-off operator
			// scripts can legitimately emit notes without a session context.
			resolvedCSID := resolveClaudeSessionID(claudeSessionID)
			if resolvedCSID != "" {
				body["claude_session_id"] = resolvedCSID
			} else {
				warnMissingSessionID(cmd.ErrOrStderr(), "improvement emit")
			}

			cl := client.New(cfg)

			return runCall(cmd.Context(), callOpts{
				cmdName:  "improvement emit",
				args:     map[string]any{"category": category, "propagation": propagation},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(body []byte) (string, error) {
					var resp struct {
						EventID string `json:"event_id"`
					}
					_ = json.Unmarshal(body, &resp)
					if resp.EventID != "" {
						return fmt.Sprintf("improvement-note emitted (category=%s, propagation=%s, event_id=%s)",
							category, propagation, resp.EventID), nil
					}
					return fmt.Sprintf("improvement-note emitted (category=%s, propagation=%s)",
						category, propagation), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				status, raw, err := cl.Do(ctx, "POST", "/v1/events", body)
				if err != nil && errors.Is(err, client.ErrSanitiserBlocked) {
					// Improvement-notes go through the same §2.1 sanitiser as
					// every other event; surface the matched-pattern detail
					// the same way `event emit` does so operators can fix the
					// offending wording without re-fetching the gateway log.
					var apiErr *client.APIError
					if errors.As(err, &apiErr) {
						return status, raw, fmt.Errorf(
							"sanitiser blocked improvement-note (matched_pattern=%q matched_field=%s blocked_event_id=%s; offending content NOT stored): %w",
							apiErr.Envelope.MatchedPattern,
							apiErr.Envelope.MatchedField,
							apiErr.Envelope.BlockedEventID,
							err,
						)
					}
					return status, raw, fmt.Errorf("sanitiser blocked improvement-note (offending content NOT stored): %w", err)
				}
				return status, raw, err
			})
		},
	}

	cmd.Flags().StringVar(&category, "category", "",
		fmt.Sprintf("category (required, one of: %s)", strings.Join(ImprovementCategories, ", ")))
	cmd.Flags().StringVar(&summary, "summary", "",
		fmt.Sprintf("short summary (required, ≤ %d chars)", improvementSummaryMaxRunes))
	cmd.Flags().StringVar(&contextStr, "context", "",
		"optional context (e.g., work-item key 'feat-04-bulk-import' or free-text 'v0.1.7 deploy smoke')")
	cmd.Flags().StringVar(&propagation, "propagation", "none",
		fmt.Sprintf("propagation hint (%s); 'fleet' is reserved and treated as 'mm' in v0.1.9", strings.Join(ImprovementPropagation, "|")))
	cmd.Flags().StringVar(&details, "details", "",
		"optional longer body; prefix with @ to read from a file (e.g. --details @./notes.md)")
	cmd.Flags().StringVar(&intent, "intent", "",
		fmt.Sprintf("event intent (one of: %s); absent = info; gateway requires role=operator for 'directive'",
			strings.Join(ValidIntents, ", ")))
	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "",
		"Claude session ID for cross-/clear handoff visibility (defaults to $CLAUDE_SESSION_ID, then to the SessionStart-written file at $CLAUDE_SESSION_ID_FILE or ~/.cache/concept-workflow/claude-session-id; empty → warn-but-continue)")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")

	return cmd
}

func containsString(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}
