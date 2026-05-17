package commands

import (
	"context"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// NewInboxCmd is the `inbox` group; today it has only one subcommand.
func NewInboxCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "inbox", Short: "Mattermost inbox subcommands"}
	cmd.AddCommand(newInboxPollCmd())
	return cmd
}

// newInboxPollCmd wraps GET /v1/inbox. Always polls for the caller's own
// agent name (operator cross-agent polling is out of scope for v0.1.0).
func newInboxPollCmd() *cobra.Command {
	var since string

	cmd := &cobra.Command{
		Use:   "poll",
		Short: "Poll inbox for new operator messages",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if since != "" {
				if _, err := time.Parse(time.RFC3339, since); err != nil {
					return err
				}
			}

			q := url.Values{}
			q.Set("agent_name", cfg.AgentName)
			if since != "" {
				q.Set("since", since)
			}

			cl := client.New(cfg)
			auditor := audit.New(cfg.AuditLog)

			return runCall(cmd.Context(), callOpts{
				cmdName:    "inbox poll",
				args:       map[string]any{"agent_name": cfg.AgentName, "since": since},
				io:         cmdIO(cmd),
				strict:     strictFlag(cmd),
				auditor:    auditor,
				pretty:     prettyFlag(cmd),
				renderRead: renderJSONResponse,
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "GET", "/v1/inbox?"+q.Encode(), nil)
			})
		},
	}

	cmd.Flags().StringVar(&since, "since", "", "only return messages newer than this RFC3339 timestamp")
	cmd.Flags().Bool("pretty", false, "indent JSON output")

	return cmd
}
