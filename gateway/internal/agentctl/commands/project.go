package commands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// NewProjectCmd is the `project` group; today it has only `register`.
// /setup-agent-events on the plugin side calls this at provisioning time so
// the consuming workspace can emit events with --project-slug X without
// hitting the gateway's "unknown_reference" 422 on the projects FK.
func NewProjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Project subcommands"}
	cmd.AddCommand(newProjectRegisterCmd())
	return cmd
}

// newProjectRegisterCmd wraps POST /v1/projects (upsert-by-slug). Mirrors
// the runCall pattern from event_emit.go: best-effort by default, audited,
// --strict elevates to exit 1.
func newProjectRegisterCmd() *cobra.Command {
	var (
		slug          string
		name          string
		forgeURL      string
		defaultBranch string
		outboxChannel string
		inboxChannel  string
	)

	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register or upsert a project in the hub (idempotent by slug)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			auditor := audit.New(cfg.AuditLog)

			if slug == "" {
				err := validationError(cmd, auditor, "project register", fmt.Errorf("--slug is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if name == "" {
				err := validationError(cmd, auditor, "project register", fmt.Errorf("--name is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			body := map[string]any{"slug": slug, "name": name}
			if forgeURL != "" {
				body["forge_url"] = forgeURL
			}
			if defaultBranch != "" {
				body["default_branch"] = defaultBranch
			}
			if outboxChannel != "" {
				body["mattermost_outbox_channel"] = outboxChannel
			}
			if inboxChannel != "" {
				body["mattermost_inbox_channel"] = inboxChannel
			}

			cl := client.New(cfg)

			return runCall(cmd.Context(), callOpts{
				cmdName:  "project register",
				args:     map[string]any{"slug": slug, "name": name},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(body []byte) (string, error) {
					var resp struct {
						ID string `json:"id"`
					}
					_ = json.Unmarshal(body, &resp)
					if resp.ID != "" {
						return fmt.Sprintf("project register: project %s registered (id=%s)", slug, shortID(resp.ID)), nil
					}
					return fmt.Sprintf("project register: project %s registered", slug), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "POST", "/v1/projects", body)
			})
		},
	}

	cmd.Flags().StringVar(&slug, "slug", "", "project slug (required, unique)")
	cmd.Flags().StringVar(&name, "name", "", "project display name (required)")
	cmd.Flags().StringVar(&forgeURL, "forge-url", "", "primary forge URL (e.g. ssh://...)")
	cmd.Flags().StringVar(&defaultBranch, "default-branch", "", "default branch (gateway defaults to 'main' if empty)")
	cmd.Flags().StringVar(&outboxChannel, "mattermost-outbox-channel", "", "Mattermost channel for outbox posts")
	cmd.Flags().StringVar(&inboxChannel, "mattermost-inbox-channel", "", "Mattermost channel for inbox webhooks")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")

	return cmd
}
