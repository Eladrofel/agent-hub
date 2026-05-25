package commands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// NewCheckpointCmd wraps POST /v1/sessions/checkpoint. The repeatable
// --next, --open-question, --file-relevant, --risk flags assemble into the
// JSON-array fields the gateway expects (next_actions, open_questions, etc).
func NewCheckpointCmd() *cobra.Command {
	var (
		claudeSessionID string
		taskKey         string
		checkpointType  string
		status          string
		currentGoal     string
		summary         string
		nextActions     []string
		openQuestions   []string
		filesRelevant   []string
		risks           []string
		payloadJSON     string
	)

	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Write a session checkpoint",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			auditor := audit.New(cfg.AuditLog)

			// v0.1.12: adopt the same flag→env precedence used by
			// `improvement emit` + `event emit` (v0.1.11 session_id.go
			// helper). Pre-v0.1.12 `checkpoint` required the explicit
			// flag, leaving in-Claude-tool callers to plumb it by hand
			// despite Claude Code already exposing $CLAUDE_SESSION_ID.
			claudeSessionID = resolveClaudeSessionID(claudeSessionID)
			if claudeSessionID == "" {
				err := validationError(cmd, auditor, "checkpoint",
					fmt.Errorf("--claude-session-id is required (or set %s)", claudeSessionIDEnv))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if summary == "" {
				err := validationError(cmd, auditor, "checkpoint", fmt.Errorf("--summary is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			body := map[string]any{
				"claude_session_id": claudeSessionID,
				"summary":           summary,
			}
			if taskKey != "" {
				body["task_key"] = taskKey
			}
			if checkpointType != "" {
				body["checkpoint_type"] = checkpointType
			}
			if status != "" {
				body["status"] = status
			}
			if currentGoal != "" {
				body["current_goal"] = currentGoal
			}
			if len(nextActions) > 0 {
				body["next_actions"] = stringsToAny(nextActions)
			}
			if len(openQuestions) > 0 {
				body["open_questions"] = stringsToAny(openQuestions)
			}
			if len(filesRelevant) > 0 {
				body["files_relevant"] = stringsToAny(filesRelevant)
			}
			if len(risks) > 0 {
				body["risks"] = stringsToAny(risks)
			}
			if payloadJSON != "" {
				var pl map[string]any
				if err := json.Unmarshal([]byte(payloadJSON), &pl); err != nil {
					return fmt.Errorf("--payload: invalid JSON: %w", err)
				}
				body["payload"] = pl
			}

			cl := client.New(cfg)

			return runCall(cmd.Context(), callOpts{
				cmdName:  "checkpoint",
				args:     map[string]any{"claude_session_id": claudeSessionID, "task_key": taskKey},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(body []byte) (string, error) {
					return fmt.Sprintf("checkpoint: wrote checkpoint for session %s", shortID(claudeSessionID)), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "POST", "/v1/sessions/checkpoint", body)
			})
		},
	}

	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "", "Claude session ID (defaults to $CLAUDE_SESSION_ID)")
	cmd.Flags().StringVar(&taskKey, "task-key", "", "legacy `tasks` table key (NOT a concept-workflow work-item key; for those use `agentctl work-item …`)")
	cmd.Flags().StringVar(&checkpointType, "checkpoint-type", "", "checkpoint type")
	cmd.Flags().StringVar(&status, "status", "", "status")
	cmd.Flags().StringVar(&currentGoal, "current-goal", "", "current goal")
	cmd.Flags().StringVar(&summary, "summary", "", "checkpoint summary (required)")
	cmd.Flags().StringArrayVar(&nextActions, "next", nil, "next action (repeatable)")
	cmd.Flags().StringArrayVar(&openQuestions, "open-question", nil, "open question (repeatable)")
	cmd.Flags().StringArrayVar(&filesRelevant, "file-relevant", nil, "relevant file (repeatable)")
	cmd.Flags().StringArrayVar(&risks, "risk", nil, "risk (repeatable)")
	cmd.Flags().StringVar(&payloadJSON, "payload", "", "extra payload as a JSON object")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")

	return cmd
}

func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
