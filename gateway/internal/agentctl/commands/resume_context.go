package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// NewResumeContextCmd wraps GET /v1/sessions/:cid/resume-context.
//
// Session-id resolution precedence (v0.1.12):
//  1. --claude-session-id flag (most explicit; bypasses fallback path).
//  2. $CLAUDE_SESSION_ID env var (set by Claude Code in tool contexts).
//  3. NEW FALLBACK — when both are empty, OR when --prior is passed:
//     call GET /v1/agents/{$AGENT_NAME}/latest-session to discover the
//     most-recent prior session, then query resume-context for it.
//     --prior passes $CLAUDE_SESSION_ID through as ?exclude=… so a freshly
//     /clear'd agent gets the SESSION-BEFORE-THIS-ONE, not its own new shell.
//
// Why the fallback exists: cross-/clear gives Claude Code a fresh
// $CLAUDE_SESSION_ID, so the prior session's resume-context is unreachable
// without manually pasting the old id. Dale's 2026-05-23 empirical test
// proved the data-plane works but the operator UX didn't — v0.1.12 closes
// the loop with a no-flag fallback the /resume-context skill can drive.
func NewResumeContextCmd() *cobra.Command {
	var (
		claudeSessionID string
		prior           bool
	)

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

			// Effective session-id from flag, then env. May be empty.
			effective := resolveClaudeSessionID(claudeSessionID)

			cl := client.New(cfg)

			// Fallback path: explicit --prior OR no session id at all.
			// --prior MUST run the fallback path even when env IS set:
			// the post-/clear case has a (useless, brand-new) env id and
			// we want the PRIOR session.
			if prior || effective == "" {
				resolved, ferr := resolveLatestSession(cmd.Context(), cl, cfg, effective, prior)
				if ferr != nil {
					err := validationError(cmd, auditor, "resume-context", ferr)
					if IsSilent(err) {
						return nil
					}
					return err
				}
				effective = resolved
			}

			return runCall(cmd.Context(), callOpts{
				cmdName:    "resume-context",
				args:       map[string]any{"claude_session_id": effective, "prior": prior},
				io:         cmdIO(cmd),
				strict:     strictFlag(cmd),
				auditor:    auditor,
				pretty:     prettyFlag(cmd),
				renderRead: renderJSONResponse,
			}, func(ctx context.Context) (int, []byte, error) {
				path := "/v1/sessions/" + effective + "/resume-context"
				return cl.Do(ctx, "GET", path, nil)
			})
		},
	}

	cmd.Flags().StringVar(&claudeSessionID, "claude-session-id", "", "Claude session ID (defaults to $CLAUDE_SESSION_ID; if empty, falls back to this agent's most-recent session)")
	cmd.Flags().BoolVar(&prior, "prior", false, "fetch the most-recent session EXCLUDING the current $CLAUDE_SESSION_ID (the post-/clear handoff case)")
	cmd.Flags().Bool("pretty", false, "indent JSON output")

	return cmd
}

// latestSessionResponse mirrors the gateway response shape for
// GET /v1/agents/{name_or_alias}/latest-session. Only the fields we need.
type latestSessionResponse struct {
	LatestSession struct {
		ClaudeSessionID string `json:"claude_session_id"`
	} `json:"latest_session"`
}

// resolveLatestSession calls GET /v1/agents/{$AGENT_NAME}/latest-session and
// returns the discovered claude_session_id. excludeID is passed through as
// ?exclude=… so --prior callers get the session BEFORE their current one.
// Halts with a user-actionable error when $AGENT_NAME is unset (the fallback
// path has no other way to know which agent's history to query).
func resolveLatestSession(ctx context.Context, cl *client.Client, cfg *config.Config, excludeID string, prior bool) (string, error) {
	agentName := cfg.AgentName
	if agentName == "" {
		// loadAuthedConfig already requires this, but the env may have
		// been unset between config-load and here in pathological cases;
		// surface a precise error instead of a confused 404.
		return "", fmt.Errorf("resume-context fallback needs %s env var (no --claude-session-id, no $%s)",
			config.EnvAgentName, claudeSessionIDEnv)
	}

	path := "/v1/agents/" + url.PathEscape(agentName) + "/latest-session"
	if prior && excludeID != "" {
		path += "?exclude=" + url.QueryEscape(excludeID)
	}

	status, body, err := cl.Do(ctx, "GET", path, nil)
	if err != nil {
		return "", fmt.Errorf("resume-context fallback: GET %s: %w", path, err)
	}
	if status != 200 {
		return "", fmt.Errorf("resume-context fallback: GET %s returned HTTP %d: %s", path, status, string(body))
	}

	var resp latestSessionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("resume-context fallback: decode response: %w", err)
	}
	if resp.LatestSession.ClaudeSessionID == "" {
		return "", fmt.Errorf("resume-context fallback: gateway returned no latest_session.claude_session_id")
	}
	return resp.LatestSession.ClaudeSessionID, nil
}

