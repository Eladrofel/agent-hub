package commands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// NewSessionStartCmd wraps POST /v1/sessions/start. The gateway auto-emits a
// session.started event after the row lands; we don't double-emit here.
func NewSessionStartCmd() *cobra.Command {
	var (
		claudeSessionID string
		projectSlug     string
		branch          string
		baseBranch      string
		cwd             string
		worktreePath    string
		vmHostname      string
		gitHeadSHA      string
		startReason     string
		metadataJSON    string
	)

	cmd := &cobra.Command{
		Use:   "session-start",
		Short: "Start a new Claude session record",
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
			if projectSlug == "" {
				projectSlug = cfg.ProjectSlug
			}

			body := map[string]any{
				"claude_session_id": claudeSessionID,
			}
			if projectSlug != "" {
				body["project_slug"] = projectSlug
			}
			if branch != "" {
				body["branch"] = branch
			}
			if baseBranch != "" {
				body["base_branch"] = baseBranch
			}
			if cwd != "" {
				body["cwd"] = cwd
			}
			if worktreePath != "" {
				body["worktree_path"] = worktreePath
			}
			if vmHostname != "" {
				body["vm_hostname"] = vmHostname
			}
			if gitHeadSHA != "" {
				body["git_head_sha"] = gitHeadSHA
			}
			if startReason != "" {
				body["start_reason"] = startReason
			}
			if metadataJSON != "" {
				var md map[string]any
				if err := json.Unmarshal([]byte(metadataJSON), &md); err != nil {
					return fmt.Errorf("--metadata: invalid JSON: %w", err)
				}
				body["metadata"] = md
			}

			cl := client.New(cfg)
			auditor := audit.New(cfg.AuditLog)

			return runCall(cmd.Context(), callOpts{
				cmdName:  "session-start",
				args:     map[string]any{"claude_session_id": claudeSessionID, "project_slug": projectSlug},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(body []byte) (string, error) {
					return fmt.Sprintf("session-start: session %s started", shortID(claudeSessionID)), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "POST", "/v1/sessions/start", body)
			})
		},
	}

	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "", "Claude session ID (required)")
	cmd.Flags().StringVar(&projectSlug, "project-slug", "", "project slug (defaults to AGENT_PROJECT_SLUG)")
	cmd.Flags().StringVar(&branch, "branch", "", "git branch")
	cmd.Flags().StringVar(&baseBranch, "base-branch", "", "git base branch")
	cmd.Flags().StringVar(&cwd, "cwd", "", "current working directory")
	cmd.Flags().StringVar(&worktreePath, "worktree-path", "", "worktree path")
	cmd.Flags().StringVar(&vmHostname, "vm-hostname", "", "VM hostname")
	cmd.Flags().StringVar(&gitHeadSHA, "git-head-sha", "", "current git HEAD SHA")
	cmd.Flags().StringVar(&startReason, "start-reason", "", "human-readable reason for session start")
	cmd.Flags().StringVar(&metadataJSON, "metadata", "", "metadata as a JSON object")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")

	return cmd
}

// shortID returns the first 8 chars of an ID for log lines; falls back to
// the full string if shorter.
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "..."
}
