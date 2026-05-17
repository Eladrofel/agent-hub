package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

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
			if eventType == "" {
				return fmt.Errorf("--type is required")
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
			if claudeSessionID != "" {
				body["claude_session_id"] = claudeSessionID
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
			if artefactJSON != "" {
				var ap map[string]any
				if err := json.Unmarshal([]byte(artefactJSON), &ap); err != nil {
					return fmt.Errorf("--artefact-pointer: invalid JSON: %w", err)
				}
				body["artefact_pointer"] = ap
			}

			cl := client.New(cfg)
			auditor := audit.New(cfg.AuditLog)

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
					// Wrap so the audit + stderr line make the cause obvious.
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
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")

	return cmd
}
