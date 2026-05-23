package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// ValidIntents is the locked enum for the v0.1.10 --intent flag exposed by
// emit-style subcommands. Threaded into the event payload at `payload.intent`;
// absent / empty means the gateway treats it as "info". Gateway-side
// enforcement (handlers_events.go) requires role=operator for intent=directive.
// See references/peer-coordination-policy.md (informational doc URL surfaced
// in the gateway's 403 response).
var ValidIntents = []string{"info", "directive", "question", "blocker", "status"}

// validateIntent checks --intent against the locked enum. Empty is allowed
// (gateway treats absent as "info"); any non-empty value MUST be in
// ValidIntents or we surface a validation error before hitting the gateway.
func validateIntent(intent string) error {
	if intent == "" {
		return nil
	}
	if !containsString(ValidIntents, intent) {
		return fmt.Errorf("--intent=%q invalid; must be one of %s",
			intent, strings.Join(ValidIntents, ", "))
	}
	return nil
}

// NewEventCmd is the `event` group; today it has only one subcommand.
func NewEventCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "event", Short: "Event subcommands"}
	cmd.AddCommand(newEventEmitCmd())
	return cmd
}

// newEventEmitCmd wraps POST /v1/events. Sanitiser-blocked responses (422
// with error="sanitiser_blocked") are surfaced as a distinct stderr line so
// the operator can tell them apart from generic errors; under best-effort
// posture, exit code stays 0 either way.
func newEventEmitCmd() *cobra.Command {
	var (
		eventType       string
		summary         string
		payloadJSON     string
		claudeSessionID string
		taskKey         string
		projectSlug     string
		branch          string
		gitHeadSHA      string
		artefactJSON    string
		correlationID   string
		causationID     string
		parentEventID   string
		actorType       string
		actorName       string
		worktreePath    string
		intent          string
	)

	cmd := &cobra.Command{
		Use:   "emit",
		Short: "Emit one event (best-effort; exit 0 on local-log fallback)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			// Auditor constructed BEFORE validation so arg-validation failures
			// are audited (validation_error outcome). See validationError doc.
			auditor := audit.New(cfg.AuditLog)

			if eventType == "" {
				err := validationError(cmd, auditor, "event emit", fmt.Errorf("--type is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if err := validateIntent(intent); err != nil {
				err := validationError(cmd, auditor, "event emit", err)
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if projectSlug == "" {
				projectSlug = cfg.ProjectSlug
			}

			body := map[string]any{"event_type": eventType}
			if summary != "" {
				body["summary"] = summary
			}
			if projectSlug != "" {
				body["project_slug"] = projectSlug
			}
			if taskKey != "" {
				body["task_key"] = taskKey
			}
			// v0.1.11: same env fallback as improvement emit. The flag wins;
			// $CLAUDE_SESSION_ID is the fallback (set by Claude Code in tool
			// contexts); empty is best-effort with a stderr warning. Tagging
			// is the load-bearing requirement for cross-/clear handoff — see
			// session_id.go for the rationale.
			resolvedCSID := resolveClaudeSessionID(claudeSessionID)
			if resolvedCSID != "" {
				body["claude_session_id"] = resolvedCSID
			} else {
				warnMissingSessionID(cmd.ErrOrStderr(), "event emit")
			}
			if branch != "" {
				body["branch"] = branch
			}
			if gitHeadSHA != "" {
				body["git_head_sha"] = gitHeadSHA
			}
			if correlationID != "" {
				body["correlation_id"] = correlationID
			}
			if causationID != "" {
				body["causation_id"] = causationID
			}
			if parentEventID != "" {
				body["parent_event_id"] = parentEventID
			}
			if actorType != "" {
				body["actor_type"] = actorType
			}
			if actorName != "" {
				body["actor_name"] = actorName
			}
			if worktreePath != "" {
				body["worktree_path"] = worktreePath
			}
			if payloadJSON != "" {
				var pl map[string]any
				if err := json.Unmarshal([]byte(payloadJSON), &pl); err != nil {
					return fmt.Errorf("--json-payload: invalid JSON: %w", err)
				}
				body["payload"] = pl
			}
			// --intent lives at payload.intent (v0.1.10). Empty omits the
			// field entirely so the gateway's "absent → info" contract
			// holds. If --json-payload also sets payload.intent the flag
			// takes precedence — the flag is the explicit signal.
			if intent != "" {
				pl, _ := body["payload"].(map[string]any)
				if pl == nil {
					pl = map[string]any{}
				}
				pl["intent"] = intent
				body["payload"] = pl
			}
			if artefactJSON != "" {
				var ap map[string]any
				if err := json.Unmarshal([]byte(artefactJSON), &ap); err != nil {
					return fmt.Errorf("--artefact-pointer: invalid JSON: %w", err)
				}
				body["artefact_pointer"] = ap
			}

			cl := client.New(cfg)

			// Special-case the sanitiser-blocked outcome so stderr lines are
			// unambiguous. The wrapper around runCall is just here to pre-
			// classify the error before runCall renders it.
			return runCall(cmd.Context(), callOpts{
				cmdName:  "event emit",
				args:     map[string]any{"event_type": eventType, "task_key": taskKey},
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
						return fmt.Sprintf("event emit: emitted %s (id=%s)", eventType, shortID(resp.EventID)), nil
					}
					return fmt.Sprintf("event emit: emitted %s", eventType), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				status, raw, err := cl.Do(ctx, "POST", "/v1/events", body)
				if err != nil && errors.Is(err, client.ErrSanitiserBlocked) {
					// Surface which §2.1 pattern fired + which field tripped
					// it, so the operator can tell apart the dozen-odd
					// patterns without reading the gateway's audit log. The
					// gateway returns these as top-level JSON fields on the
					// 422 response (sanitiserBlockedResponse). We never echo
					// the offending content itself — that's the whole point
					// of the block.
					var apiErr *client.APIError
					if errors.As(err, &apiErr) {
						return status, raw, fmt.Errorf(
							"sanitiser blocked event_type=%s (matched_pattern=%q matched_field=%s blocked_event_id=%s; offending content NOT stored): %w",
							eventType,
							apiErr.Envelope.MatchedPattern,
							apiErr.Envelope.MatchedField,
							apiErr.Envelope.BlockedEventID,
							err,
						)
					}
					// Defensive fallback — Do() should always wrap non-2xx in
					// *APIError, so this branch is unreachable unless the
					// client contract changes.
					return status, raw, fmt.Errorf("sanitiser blocked event_type=%s (offending content NOT stored): %w", eventType, err)
				}
				return status, raw, err
			})
		},
	}

	cmd.Flags().StringVar(&eventType, "type", "", "event type (required)")
	cmd.Flags().StringVar(&summary, "summary", "", "human-readable summary")
	cmd.Flags().StringVar(&payloadJSON, "json-payload", "", "payload as a JSON object")
	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "", "Claude session ID")
	cmd.Flags().StringVar(&taskKey, "task-key", "", "task key (e.g. feat-01-landing-page)")
	cmd.Flags().StringVar(&projectSlug, "project-slug", "", "project slug (defaults to AGENT_PROJECT_SLUG)")
	cmd.Flags().StringVar(&branch, "branch", "", "git branch")
	cmd.Flags().StringVar(&gitHeadSHA, "git-head-sha", "", "git HEAD SHA")
	cmd.Flags().StringVar(&artefactJSON, "artefact-pointer", "", "artefact pointer as a JSON object")
	cmd.Flags().StringVar(&correlationID, "correlation-id", "", "correlation ID")
	cmd.Flags().StringVar(&causationID, "causation-id", "", "causation ID")
	cmd.Flags().StringVar(&parentEventID, "parent-event-id", "", "parent event ID")
	cmd.Flags().StringVar(&actorType, "actor-type", "", "actor type")
	cmd.Flags().StringVar(&actorName, "actor-name", "", "actor name")
	cmd.Flags().StringVar(&worktreePath, "worktree-path", "", "worktree path")
	cmd.Flags().StringVar(&intent, "intent", "",
		fmt.Sprintf("event intent (one of: %s); absent = info; gateway requires role=operator for 'directive'",
			strings.Join(ValidIntents, ", ")))
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")

	return cmd
}
