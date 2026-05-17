// agent-hub is the server-side binary that runs inside docker-compose.
// One image, three roles selected by subcommand:
//
//	agent-hub serve          — HTTP gateway on $LISTEN_ADDR (default :8787)
//	agent-hub outbox-worker  — polls mattermost_outbox, posts via Mattermost MCP
//	agent-hub inbox-webhook  — HTTP receiver on $LISTEN_ADDR (default :8788) for
//	                           Mattermost outgoing webhooks → mattermost_inbox
//	agent-hub migrate        — apply embedded SQL migrations idempotently
//
// Environment:
//
//	DATABASE_URL            — Postgres DSN
//	LISTEN_ADDR             — bind address (defaults: :8787 serve, :8788 inbox)
//	ADMIN_TOKEN             — used by /v1/admin/* endpoints (mint tokens, etc.)
//	SANITISER_PATTERNS_FILE — path to §2.1 leak-pattern file
//	MATTERMOST_URL          — outbox-worker target (v0.1.1+)
//	MATTERMOST_TOKEN        — outbox-worker service-account PAT (v0.1.1+)
//	POLL_INTERVAL_SECONDS   — outbox-worker poll cadence (v0.1.1+)
//	WEBHOOK_SECRET          — inbox-webhook shared secret (v0.1.1+)
//
// v0.1.0 ships `serve` + `migrate`. outbox-worker and inbox-webhook are
// v0.1.1.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/server"
	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

var (
	version = "0.1.0"
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
			cfg := server.Config{
				ListenAddr:            envOr("LISTEN_ADDR", ":8787"),
				DatabaseURL:           os.Getenv("DATABASE_URL"),
				AdminToken:            os.Getenv("ADMIN_TOKEN"),
				SanitiserPatternsFile: envOr("SANITISER_PATTERNS_FILE", "/etc/agent-hub/sanitiser-patterns.txt"),
			}
			if cfg.DatabaseURL == "" {
				return fmt.Errorf("DATABASE_URL is required")
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return server.Run(ctx, cfg)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "migrate",
		Short: "Apply embedded SQL migrations (idempotent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			dsn := os.Getenv("DATABASE_URL")
			if dsn == "" {
				return fmt.Errorf("DATABASE_URL is required")
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			st, err := store.Open(ctx, dsn)
			if err != nil {
				return err
			}
			defer st.Close()
			return st.Migrate(ctx)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "outbox-worker",
		Short: "Poll mattermost_outbox + post to Mattermost (v0.1.1)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("outbox-worker: not implemented in v0.1.0; ships in v0.1.1 alongside ROADMAP #10 Component C")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "inbox-webhook",
		Short: "Receive Mattermost outgoing webhooks → mattermost_inbox (v0.1.1)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("inbox-webhook: not implemented in v0.1.0; ships in v0.1.1 alongside ROADMAP #10 Component C")
		},
	})

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
