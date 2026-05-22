// agent-hub is the server-side binary that runs inside docker-compose.
// One image, three roles selected by subcommand:
//
//	agent-hub serve          — HTTP gateway on $LISTEN_ADDR (default :8787)
//	agent-hub outbox-worker  — polls mattermost_outbox, posts via Mattermost REST API
//	agent-hub inbox-webhook  — HTTP receiver on $LISTEN_ADDR (default :8788) for
//	                           Mattermost outgoing webhooks → mattermost_inbox
//	agent-hub migrate        — apply embedded SQL migrations idempotently
//
// Environment (full set; subcommand-specific subset documented per command):
//
//	DATABASE_URL                       — Postgres DSN (all subcommands)
//	LISTEN_ADDR                        — bind address (defaults: :8787 serve, :8788 inbox)
//	ADMIN_TOKEN                        — used by /v1/admin/* endpoints (serve)
//	SANITISER_PATTERNS_FILE            — §2.1 leak-pattern file (serve)
//	SANITISER_EXEMPT_HOSTS             — comma-sep hosts exempt from §2.1 (serve, v0.1.3 task #29)
//	MATTERMOST_URL                     — Mattermost base URL (outbox-worker)
//	MATTERMOST_TOKEN                   — service-account PAT (outbox-worker)
//	MATTERMOST_TEAM_NAME               — team for channel-name resolution (outbox-worker, v0.1.3)
//	MATTERMOST_DEFAULT_OUTBOX_CHANNEL  — default channel when project row has none (serve)
//	POLL_INTERVAL_SECONDS              — outbox-worker poll cadence (default 5)
//	WEBHOOK_SECRET                     — inbox-webhook shared secret
//
// v0.1.0 ships `serve` + `migrate`. v0.1.3 ships outbox-worker + inbox-webhook
// as part of ROADMAP #10 Component C (Mattermost bidirectional flow).

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/inbox"
	"github.com/Eladrofel/agent-hub/gateway/internal/outbox"
	"github.com/Eladrofel/agent-hub/gateway/internal/server"
	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

var (
	version = "0.1.9"
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
				ListenAddr:              envOr("LISTEN_ADDR", ":8787"),
				DatabaseURL:             os.Getenv("DATABASE_URL"),
				AdminToken:              os.Getenv("ADMIN_TOKEN"),
				SanitiserPatternsFile:   envOr("SANITISER_PATTERNS_FILE", "/etc/agent-hub/sanitiser-patterns.txt"),
				SanitiserExemptHosts:    splitCSV(os.Getenv("SANITISER_EXEMPT_HOSTS")),
				MattermostDefaultOutbox: envOr("MATTERMOST_DEFAULT_OUTBOX_CHANNEL", "agent-events"),
				Version:                 fmt.Sprintf("v%s", version),
				DistDir:                 envOr("AGENT_HUB_DIST_DIR", "/opt/agent-hub/dist"),
				GatewayURL:              os.Getenv("AGENT_HUB_GATEWAY_URL"),
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
		Short: "Poll mattermost_outbox + post to Mattermost (v0.1.3 Component C)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := outbox.DefaultWorkerConfig()
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return outbox.Run(ctx, cfg)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "inbox-webhook",
		Short: "Receive Mattermost outgoing webhooks → mattermost_inbox (v0.1.3 Component C)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := inbox.DefaultWebhookConfig()
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return inbox.RunWebhook(ctx, cfg)
		},
	})

	printTokens := &cobra.Command{
		Use:   "print-tokens",
		Short: "Print the gateway's join-code HMAC key + mint-authority token (v0.4.0)",
		Long: `Reads the v0.4.0 federated-trust secrets (JOIN_CODE_HMAC_KEY +
MINT_AUTHORITY_TOKEN) from the kv_store table, where they're persisted
on first boot if no env-var override was set.

Intended for ` + "`docker exec agent-hub agent-hub print-tokens`" + ` so the
operator can retrieve the auto-generated secrets after first boot.
Refuses to run unless stdout looks like a terminal (TERM set) so
secrets don't accidentally leak into log scrapers; pass --force to
override.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			if !force && os.Getenv("TERM") == "" {
				return fmt.Errorf("refusing to print secrets: stdout not a terminal (TERM unset); pass --force to override")
			}
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
			return server.PrintTokens(ctx, st, cmd.OutOrStdout())
		},
	}
	printTokens.Flags().Bool("force", false, "print secrets even if stdout is not a terminal")
	root.AddCommand(printTokens)

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

// splitCSV trims and drops empties so an unset env var (which Split yields
// as [""]) doesn't propagate a phantom empty entry into downstream config.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
