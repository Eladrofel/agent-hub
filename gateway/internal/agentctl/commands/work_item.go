// work_item.go — `agentctl work-item {claim,finish,active}` subcommand group
// (v0.1.14).
//
// Concept-workflow work-items (feat-04-bulk-import, bugfix-12-…) historically
// had no durable record of "who's on it right now" — the only signal was a
// Mode-3-only Mattermost heads-up emitted by /start-work-item Phase 1.2. Two
// agents on different VMs could race the same work-item without either knowing.
//
// v0.1.14 closes the gap with a claim/finish event pair and an agent-readable
// active-claims read endpoint. The plugin's /start-work-item runs `active` as
// a pre-flight, halts on conflict, and writes `claim` before branching. The
// plugin's /finish-work-item writes `finish` after PR-open. Both events are
// curated → auto-relay to Mattermost for human visibility.
//
// All three verbs share the existing audit + strict-mode + claude_session_id
// plumbing used by `agentctl improvement emit` and `agentctl checkpoint`.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

const (
	workItemClaimedEventType  = "agent.work-item.claimed"
	workItemFinishedEventType = "agent.work-item.finished"
)

// NewWorkItemCmd is the `work-item` group. Multi-verb (claim / finish / active),
// mirrors the improvement.go pattern. Wired into root in cmd/agentctl/main.go.
func NewWorkItemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "work-item",
		Short: "Work-item peer-coordination (claim / finish / active)",
	}
	cmd.AddCommand(newWorkItemClaimCmd())
	cmd.AddCommand(newWorkItemFinishCmd())
	cmd.AddCommand(newWorkItemActiveCmd())
	return cmd
}

func newWorkItemClaimCmd() *cobra.Command {
	var (
		wiKey           string
		repo            string
		branch          string
		force           bool
		claudeSessionID string
	)

	cmd := &cobra.Command{
		Use:   "claim",
		Short: "Claim a work-item (writes agent.work-item.claimed event)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			auditor := audit.New(cfg.AuditLog)

			wiKey = strings.TrimSpace(wiKey)
			if wiKey == "" {
				err := validationError(cmd, auditor, "work-item claim", fmt.Errorf("--wi-key is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if strings.TrimSpace(repo) == "" {
				err := validationError(cmd, auditor, "work-item claim", fmt.Errorf("--repo is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			payload := map[string]any{
				"wi_key": wiKey,
				"repo":   repo,
				"force":  force,
			}
			if branch != "" {
				payload["branch"] = branch
			}

			summary := fmt.Sprintf("claimed %s (%s)", wiKey, repo)
			if force {
				summary += " [forced]"
			}

			body := map[string]any{
				"event_type": workItemClaimedEventType,
				"summary":    summary,
				"payload":    payload,
			}
			if cfg.ProjectSlug != "" {
				body["project_slug"] = cfg.ProjectSlug
			}
			if csid := resolveClaudeSessionID(claudeSessionID); csid != "" {
				body["claude_session_id"] = csid
			} else {
				warnMissingSessionID(cmd.ErrOrStderr(), "work-item claim")
			}

			cl := client.New(cfg)
			return runCall(cmd.Context(), callOpts{
				cmdName:  "work-item claim",
				args:     map[string]any{"wi_key": wiKey, "repo": repo, "force": force},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(respBody []byte) (string, error) {
					var resp struct {
						EventID string `json:"event_id"`
					}
					_ = json.Unmarshal(respBody, &resp)
					if resp.EventID != "" {
						return fmt.Sprintf("work-item claim emitted (wi_key=%s, repo=%s, event_id=%s)",
							wiKey, repo, resp.EventID), nil
					}
					return fmt.Sprintf("work-item claim emitted (wi_key=%s, repo=%s)", wiKey, repo), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "POST", "/v1/events", body)
			})
		},
	}

	cmd.Flags().StringVar(&wiKey, "wi-key", "", "work-item key (required, e.g. feat-04-bulk-import)")
	cmd.Flags().StringVar(&repo, "repo", "", "workspace submodule name (required, e.g. customer-web)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch name being created for this work-item (optional)")
	cmd.Flags().BoolVar(&force, "force", false, "write a competing claim even if active claims exist (audit-visible)")
	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "",
		"Claude session ID (defaults to $CLAUDE_SESSION_ID, then to the SessionStart-written file at $CLAUDE_SESSION_ID_FILE or ~/.cache/concept-workflow/claude-session-id; empty → warn-but-continue)")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")
	return cmd
}

func newWorkItemFinishCmd() *cobra.Command {
	var (
		wiKey           string
		repo            string
		prURL           string
		claudeSessionID string
	)

	cmd := &cobra.Command{
		Use:   "finish",
		Short: "Finish a work-item (writes agent.work-item.finished event)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			auditor := audit.New(cfg.AuditLog)

			wiKey = strings.TrimSpace(wiKey)
			if wiKey == "" {
				err := validationError(cmd, auditor, "work-item finish", fmt.Errorf("--wi-key is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if strings.TrimSpace(repo) == "" {
				err := validationError(cmd, auditor, "work-item finish", fmt.Errorf("--repo is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			payload := map[string]any{
				"wi_key": wiKey,
				"repo":   repo,
			}
			if prURL != "" {
				payload["pr_url"] = prURL
			}

			summary := fmt.Sprintf("finished %s (%s)", wiKey, repo)
			if prURL != "" {
				summary += " — " + prURL
			}

			body := map[string]any{
				"event_type": workItemFinishedEventType,
				"summary":    summary,
				"payload":    payload,
			}
			if cfg.ProjectSlug != "" {
				body["project_slug"] = cfg.ProjectSlug
			}
			if csid := resolveClaudeSessionID(claudeSessionID); csid != "" {
				body["claude_session_id"] = csid
			} else {
				warnMissingSessionID(cmd.ErrOrStderr(), "work-item finish")
			}

			cl := client.New(cfg)
			return runCall(cmd.Context(), callOpts{
				cmdName:  "work-item finish",
				args:     map[string]any{"wi_key": wiKey, "repo": repo, "pr_url": prURL},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(respBody []byte) (string, error) {
					var resp struct {
						EventID string `json:"event_id"`
					}
					_ = json.Unmarshal(respBody, &resp)
					if resp.EventID != "" {
						return fmt.Sprintf("work-item finish emitted (wi_key=%s, repo=%s, event_id=%s)",
							wiKey, repo, resp.EventID), nil
					}
					return fmt.Sprintf("work-item finish emitted (wi_key=%s, repo=%s)", wiKey, repo), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "POST", "/v1/events", body)
			})
		},
	}

	cmd.Flags().StringVar(&wiKey, "wi-key", "", "work-item key (required)")
	cmd.Flags().StringVar(&repo, "repo", "", "workspace submodule name (required)")
	cmd.Flags().StringVar(&prURL, "pr-url", "", "pull-request URL just opened (optional)")
	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "",
		"Claude session ID (defaults to $CLAUDE_SESSION_ID, then to the SessionStart-written file at $CLAUDE_SESSION_ID_FILE or ~/.cache/concept-workflow/claude-session-id; empty → warn-but-continue)")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")
	return cmd
}

func newWorkItemActiveCmd() *cobra.Command {
	var wiKey string

	cmd := &cobra.Command{
		Use:   "active",
		Short: "List active (unfinished) claims on a work-item",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			auditor := audit.New(cfg.AuditLog)

			wiKey = strings.TrimSpace(wiKey)
			if wiKey == "" {
				err := validationError(cmd, auditor, "work-item active", fmt.Errorf("--wi-key is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			cl := client.New(cfg)
			path := "/v1/work-items/" + url.PathEscape(wiKey) + "/active-claims"
			if cfg.ProjectSlug != "" {
				path += "?project_slug=" + url.QueryEscape(cfg.ProjectSlug)
			}

			return runCall(cmd.Context(), callOpts{
				cmdName:    "work-item active",
				args:       map[string]any{"wi_key": wiKey, "project_slug": cfg.ProjectSlug},
				io:         cmdIO(cmd),
				strict:     strictFlag(cmd),
				auditor:    auditor,
				pretty:     prettyFlag(cmd),
				renderRead: renderJSONResponse,
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "GET", path, nil)
			})
		},
	}

	cmd.Flags().StringVar(&wiKey, "wi-key", "", "work-item key (required)")
	cmd.Flags().Bool("pretty", false, "indent JSON output")
	return cmd
}
