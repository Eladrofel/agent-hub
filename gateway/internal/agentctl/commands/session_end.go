package commands

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// NewSessionEndCmd wraps POST /v1/sessions/end. The gateway auto-emits a
// session.ended event after the row updates.
func NewSessionEndCmd() *cobra.Command {
	var (
		claudeSessionID string
		finalStatus     string
	)

	cmd := &cobra.Command{
		Use:   "session-end",
		Short: "End the current Claude session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if claudeSessionID == "" {
				return fmt.Errorf("--claude-session-id is required")
			}

			body := map[string]any{
				"claude_session_id": claudeSessionID,
			}
			if finalStatus != "" {
				body["final_status"] = finalStatus
			}

			cl := client.New(cfg)
			auditor := audit.New(cfg.AuditLog)

			return runCall(cmd.Context(), callOpts{
				cmdName:  "session-end",
				args:     map[string]any{"claude_session_id": claudeSessionID, "final_status": finalStatus},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(body []byte) (string, error) {
					return fmt.Sprintf("session-end: session %s ended", shortID(claudeSessionID)), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "POST", "/v1/sessions/end", body)
			})
		},
	}

	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "", "Claude session ID (required)")
	cmd.Flags().StringVar(&finalStatus, "final-status", "", "final session status (e.g. task_completed)")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")

	return cmd
}
