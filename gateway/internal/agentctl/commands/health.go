package commands

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// NewHealthCmd wraps GET /health. Unlike the other subcommands, health is
// always strict (an explicit health check that exits 0 on failure would be
// useless), and it requires only AGENT_HUB_URL.
func NewHealthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Report gateway connectivity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(false)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl health: %v\n", err)
				return err
			}

			cl := client.New(cfg)
			auditor := audit.New(cfg.AuditLog)

			// Force strict regardless of flag: health checks ALWAYS report
			// failure via exit code. Best-effort posture does not apply.
			return runCall(cmd.Context(), callOpts{
				cmdName:    "health",
				args:       map[string]any{"url": cfg.URL},
				io:         cmdIO(cmd),
				strict:     true,
				auditor:    auditor,
				pretty:     prettyFlag(cmd),
				renderRead: renderJSONResponse,
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "GET", "/health", nil)
			})
		},
	}

	cmd.Flags().Bool("pretty", false, "indent JSON output")

	return cmd
}
