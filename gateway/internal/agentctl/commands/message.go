// message.go — `agentctl message <peer-alias>` subcommand (v0.1.18).
//
// Closes the chat-emit ↔ agent-events two-surface gap empirically observed
// on 2026-05-28: agents using /chat-emit for peer-targeted communication
// produced MM posts that didn't lead with @, so the MM outgoing-webhook
// never fired, so the recipient's mattermost_inbox never got the row, so
// the recipient's /agent-inbox poll returned empty even though the message
// existed in MM. Operator had to play man-in-the-middle every time.
//
// This subcommand emits a curated agent.peer-message event with the
// recipient's alias in payload.target_agent. The gateway-side formatter
// (handlers_events_format.go) prepends @<alias> to the chat-relay line,
// which fires the outgoing webhook and routes the post into the recipient's
// inbox automatically. v0.5.6's mid-session inbox poll hook then surfaces
// the message to the recipient agent within the throttle window.
//
// One round trip; durable in Postgres; visible in MM; surfaces to the peer
// without operator mediation.
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

const peerMessageEventType = "agent.peer-message"

// peerMessageSummaryMaxRunes caps the chat-side summary the same way
// improvement emit caps it — MM-post-friendly one-screen-readable. Details
// (uncapped) carry the longer body.
const peerMessageSummaryMaxRunes = 280

// peerMessageValidIntents is the allowed set for the --intent flag. Mirrors
// the gateway-side validIntents in handlers_events.go (validation runs both
// client-side for fast-fail UX and server-side for trust-boundary).
// `directive` is gateway-gated to role=operator; non-operator senders that
// pass --intent directive will get HTTP 403 from the gateway.
var peerMessageValidIntents = []string{"info", "directive", "question", "blocker", "status"}

// NewMessageCmd is the `agentctl message <peer-alias>` subcommand. Wired
// into root in cmd/agentctl/main.go.
//
// Positional first argument is the recipient's MM alias (Donnie/Mikey/etc.);
// case-insensitive match per v0.1.8 #45. Required flags: --summary (≤280
// chars). Optional: --intent (default info), --details (longer body; @file
// supported like improvement emit).
func NewMessageCmd() *cobra.Command {
	var (
		intent          string
		summary         string
		details         string
		claudeSessionID string
	)

	cmd := &cobra.Command{
		Use:   "message <peer-alias>",
		Short: "Send a peer-targeted message (durable event + auto-routed MM @-mention)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			auditor := audit.New(cfg.AuditLog)

			targetAlias := strings.TrimSpace(args[0])
			if targetAlias == "" {
				err := validationError(cmd, auditor, "message", fmt.Errorf("peer-alias positional argument is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			if intent == "" {
				intent = "info"
			}
			if !containsString(peerMessageValidIntents, intent) {
				err := validationError(cmd, auditor, "message",
					fmt.Errorf("--intent=%q invalid; must be one of %s", intent, strings.Join(peerMessageValidIntents, ", ")))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			summary = strings.TrimSpace(summary)
			if summary == "" {
				err := validationError(cmd, auditor, "message",
					fmt.Errorf("--summary is required (non-empty, ≤ %d chars)", peerMessageSummaryMaxRunes))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if n := utf8.RuneCountInString(summary); n > peerMessageSummaryMaxRunes {
				err := validationError(cmd, auditor, "message",
					fmt.Errorf("--summary is %d chars; max is %d (use --details for longer body)", n, peerMessageSummaryMaxRunes))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			// --details @file pattern (same as improvement emit).
			if strings.HasPrefix(details, "@") {
				path := details[1:]
				raw, rerr := os.ReadFile(path)
				if rerr != nil {
					err := validationError(cmd, auditor, "message",
						fmt.Errorf("--details @%s: %w", path, rerr))
					if IsSilent(err) {
						return nil
					}
					return err
				}
				details = string(raw)
			}

			payload := map[string]any{
				"target_agent": targetAlias,
				"intent":       intent,
				"summary":      summary,
			}
			if details != "" {
				payload["details"] = details
			}

			body := map[string]any{
				"event_type": peerMessageEventType,
				"summary":    summary, // top-level too so list queries don't need a JSONB deref
				"payload":    payload,
			}
			if cfg.ProjectSlug != "" {
				body["project_slug"] = cfg.ProjectSlug
			}
			if csid := resolveClaudeSessionID(claudeSessionID); csid != "" {
				body["claude_session_id"] = csid
			} else {
				warnMissingSessionID(cmd.ErrOrStderr(), "message")
			}

			cl := client.New(cfg)
			return runCall(cmd.Context(), callOpts{
				cmdName:  "message",
				args:     map[string]any{"target_agent": targetAlias, "intent": intent},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(respBody []byte) (string, error) {
					var resp struct {
						EventID string `json:"event_id"`
					}
					_ = json.Unmarshal(respBody, &resp)
					if resp.EventID != "" {
						return fmt.Sprintf("message emitted to %s (intent=%s, event_id=%s)",
							targetAlias, intent, resp.EventID), nil
					}
					return fmt.Sprintf("message emitted to %s (intent=%s)", targetAlias, intent), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				status, raw, err := cl.Do(ctx, "POST", "/v1/events", body)
				if err != nil && errors.Is(err, client.ErrSanitiserBlocked) {
					var apiErr *client.APIError
					if errors.As(err, &apiErr) {
						return status, raw, fmt.Errorf(
							"sanitiser blocked peer-message (matched_pattern=%q matched_field=%s blocked_event_id=%s; offending content NOT stored): %w",
							apiErr.Envelope.MatchedPattern,
							apiErr.Envelope.MatchedField,
							apiErr.Envelope.BlockedEventID,
							err,
						)
					}
					return status, raw, fmt.Errorf("sanitiser blocked peer-message (offending content NOT stored): %w", err)
				}
				return status, raw, err
			})
		},
	}

	cmd.Flags().StringVar(&intent, "intent", "",
		fmt.Sprintf("intent (one of: %s); default info; gateway requires role=operator for directive",
			strings.Join(peerMessageValidIntents, ", ")))
	cmd.Flags().StringVar(&summary, "summary", "",
		fmt.Sprintf("short summary (required, ≤ %d chars)", peerMessageSummaryMaxRunes))
	cmd.Flags().StringVar(&details, "details", "",
		"optional longer body; prefix with @ to read from a file (e.g. --details @./suggestion.md)")
	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "",
		"Claude session ID (defaults to $CLAUDE_SESSION_ID, then to the SessionStart-written file at $CLAUDE_SESSION_ID_FILE or ~/.cache/concept-workflow/claude-session-id)")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")
	return cmd
}
