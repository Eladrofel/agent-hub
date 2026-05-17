package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// NewResumeContextCmd wraps GET /v1/sessions/:cid/resume-context. If
// --claude-session-id is omitted, falls back to the CLAUDE_SESSION_ID env
// var (which Claude Code sets in tool contexts).
func NewResumeContextCmd() *cobra.Command {
	var claudeSessionID string

	cmd := &cobra.Command{
		Use:   "resume-context",
		Short: "Fetch the resume packet for this agent/session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			auditor := audit.New(cfg.AuditLog)

			if claudeSessionID == "" {
				claudeSessionID = os.Getenv("CLAUDE_SESSION_ID")
			}
			if claudeSessionID == "" {
				err := validationError(cmd, auditor, "resume-context", fmt.Errorf("--claude-session-id is required (or set CLAUDE_SESSION_ID env)"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			cl := client.New(cfg)

			return runCall(cmd.Context(), callOpts{
				cmdName:    "resume-context",
				args:       map[string]any{"claude_session_id": claudeSessionID},
				io:         cmdIO(cmd),
				strict:     strictFlag(cmd),
				auditor:    auditor,
				pretty:     prettyFlag(cmd),
				renderRead: renderJSONResponse,
			}, func(ctx context.Context) (int, []byte, error) {
				path := "/v1/sessions/" + claudeSessionID + "/resume-context"
				return cl.Do(ctx, "GET", path, nil)
			})
		},
	}

	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "", "Claude session ID (defaults to $CLAUDE_SESSION_ID)")
	cmd.Flags().Bool("pretty", false, "indent JSON output")

	return cmd
}
