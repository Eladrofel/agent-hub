package commands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
)

// NewRegisterAgentCmd wraps POST /v1/agents/register. The --name MUST equal
// the AGENT_NAME env var (the gateway enforces it server-side too; we check
// up-front to give a clearer error).
func NewRegisterAgentCmd() *cobra.Command {
	var (
		name         string
		role         string
		hostKind     string
		vmHostname   string
		mmUsername   string
		capabilities []string
		metadataJSON string
	)

	cmd := &cobra.Command{
		Use:   "register-agent",
		Short: "Register or upsert this agent in the hub",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadAuthedConfig(cmd, true)
			if err != nil {
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if name == "" {
				name = cfg.AgentName
			}
			if name != cfg.AgentName {
				err := fmt.Errorf("--name %q must equal AGENT_NAME %q (gateway rejects mismatch)", name, cfg.AgentName)
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl register-agent: %v\n", err)
				if strictFlag(cmd) {
					return err
				}
				return nil
			}

			body := map[string]any{"name": name}
			if role != "" {
				body["role"] = role
			}
			if hostKind != "" {
				body["host_kind"] = hostKind
			}
			if vmHostname != "" {
				body["vm_hostname"] = vmHostname
			}
			if mmUsername != "" {
				body["mattermost_username"] = mmUsername
			}
			if len(capabilities) > 0 {
				caps := make([]any, len(capabilities))
				for i, c := range capabilities {
					caps[i] = c
				}
				body["capabilities"] = caps
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
				cmdName:  "register-agent",
				args:     map[string]any{"name": name, "role": role, "host_kind": hostKind},
				io:       cmdIO(cmd),
				strict:   strictFlag(cmd),
				auditor:  auditor,
				emitJSON: jsonFlag(cmd),
				renderMutate: func(body []byte) (string, error) {
					return fmt.Sprintf("register-agent: agent %s registered", name), nil
				},
			}, func(ctx context.Context) (int, []byte, error) {
				return cl.Do(ctx, "POST", "/v1/agents/register", body)
			})
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "agent name (defaults to AGENT_NAME)")
	cmd.Flags().StringVar(&role, "role", "", "agent role (e.g. operator, agent, sub-agent)")
	cmd.Flags().StringVar(&hostKind, "host-kind", "", "host kind (e.g. linux-vm, mac-host)")
	cmd.Flags().StringVar(&vmHostname, "vm-hostname", "", "VM hostname")
	cmd.Flags().StringVar(&mmUsername, "mattermost-username", "", "Mattermost username")
	cmd.Flags().StringSliceVar(&capabilities, "capabilities", nil, "capabilities (repeatable)")
	cmd.Flags().StringVar(&metadataJSON, "metadata", "", "metadata as a JSON object")
	cmd.Flags().Bool("json", false, "emit the full response body on stdout (default: stderr summary)")

	return cmd
}
