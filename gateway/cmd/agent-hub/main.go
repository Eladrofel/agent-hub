// agent-hub is the server-side binary that runs inside docker-compose.
// One image, three roles selected by subcommand:
//
//   agent-hub serve          — HTTP gateway on $LISTEN_ADDR (default :8787)
//   agent-hub outbox-worker  — polls mattermost_outbox, posts via Mattermost MCP
//   agent-hub inbox-webhook  — HTTP receiver on $LISTEN_ADDR (default :8788) for
//                              Mattermost outgoing webhooks → mattermost_inbox
//
// Environment:
//   DATABASE_URL            — Postgres DSN
//   LISTEN_ADDR             — bind address (defaults: :8787 serve, :8788 inbox)
//   ADMIN_TOKEN             — used by /v1/admin/* endpoints (mint tokens, etc.)
//   SANITISER_PATTERNS_FILE — path to §2.1 leak-pattern file
//   MATTERMOST_URL          — outbox-worker target
//   MATTERMOST_TOKEN        — outbox-worker service-account PAT
//   POLL_INTERVAL_SECONDS   — outbox-worker poll cadence
//   WEBHOOK_SECRET          — inbox-webhook shared secret
//
// v0.1.0 ships the subcommand routing + Postgres connectivity. Endpoint
// implementations are stubbed; flesh out in v0.1.x patches.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	version = "0.1.0-dev"
	commit  = "unknown"
)

func main() {
	root := &cobra.Command{
		Use:     "agent-hub",
		Short:   "agent-events server (gateway / outbox-worker / inbox-webhook)",
		Version: fmt.Sprintf("%s (commit %s)", version, commit),
	}

	root.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("serve: not implemented; v0.1.0 stub. Stand up endpoints from gateway/internal/server.")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "outbox-worker",
		Short: "Poll mattermost_outbox + post to Mattermost",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("outbox-worker: not implemented; v0.1.0 stub. See gateway/internal/outbox.")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "inbox-webhook",
		Short: "Receive Mattermost outgoing webhooks → mattermost_inbox",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("inbox-webhook: not implemented; v0.1.0 stub. See gateway/internal/inbox.")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "migrate",
		Short: "Apply pending Postgres migrations (idempotent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("migrate: not implemented; v0.1.0 stub. Apply via psql for now: psql $DATABASE_URL -f db/migrations/001_init.sql")
		},
	})

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
