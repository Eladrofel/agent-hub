// agentctl is the CLI invoked by VM agents and the operator's Mac to talk to
// the agent-hub gateway over HTTP. Distributed as a single static binary
// cross-compiled for darwin-arm64 (operator Mac) and linux-amd64 (agent VMs).
//
// Subcommands cover the lifecycle:
//
//	agentctl register-agent --name <name> --role <role> [--host-kind <kind>]
//	agentctl session-start --claude-session-id <id> [...]
//	agentctl session-end --claude-session-id <id>
//	agentctl event emit --type <type> --summary <s> [--json-payload <payload>]
//	agentctl improvement emit --category <c> --summary <s> [--context <c>] [--propagation <p>] [--details <text-or-@file>]
//	agentctl work-item claim --wi-key <k> --repo <r> [--branch <b>] [--force]
//	agentctl work-item finish --wi-key <k> --repo <r> [--pr-url <u>]
//	agentctl work-item active --wi-key <k> [--pretty]
//	agentctl checkpoint --claude-session-id <id> --summary <s> [--next <n>...]
//	agentctl resume-context [--claude-session-id <id>]
//	agentctl inbox poll [--since <ts>]
//	agentctl health
//
// Configuration (env):
//
//	AGENT_HUB_URL         — gateway base URL (e.g., http://10.0.5.50:8787)
//	AGENT_HUB_TOKEN_FILE  — path to the per-host token file (chmod 600 on Mac;
//	                        systemd-creds-decrypted file on Linux VMs)
//	AGENT_NAME            — this agent's identity (e.g., "agent-1")
//	AGENT_PROJECT_SLUG    — default project for events that don't specify one
//	AGENT_HUB_AUDIT_LOG   — append-only JSONL audit (default
//	                        $HOME/.local/state/agent-events/audit.log)
//
// Best-effort posture (default): on any error a subcommand appends an audit
// entry, logs one line to stderr, and exits 0. The --strict flag overrides
// this to exit 1 for hard-fail callers (the orchestrator's review-posted
// step). See internal/agentctl/commands/common.go for the implementation.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/commands"
)

var (
	// agentctl wire-up to the gateway endpoints ships in v0.1.0; the binary
	// version tracks the agent-hub release line.
	version = "0.1.9"
	commit  = "unknown"
)

func main() {
	root := &cobra.Command{
		Use:           "agentctl",
		Short:         "CLI for the agent-hub gateway",
		Version:       fmt.Sprintf("%s (commit %s)", version, commit),
		SilenceUsage:  true, // we already print errors ourselves
		SilenceErrors: true,
	}

	// --strict is persistent so any subcommand can read it.
	root.PersistentFlags().Bool("strict", false,
		"exit 1 on any error (default: best-effort, exit 0 with stderr log)")

	root.AddCommand(commands.NewRegisterAgentCmd())
	root.AddCommand(commands.NewSessionStartCmd())
	root.AddCommand(commands.NewSessionEndCmd())
	root.AddCommand(commands.NewEventCmd())
	root.AddCommand(commands.NewCheckpointCmd())
	root.AddCommand(commands.NewResumeContextCmd())
	root.AddCommand(commands.NewInboxCmd())
	root.AddCommand(commands.NewHealthCmd())
	root.AddCommand(commands.NewProjectCmd())
	root.AddCommand(commands.NewJoinCmd())
	root.AddCommand(commands.NewCommsJoinCmd())
	root.AddCommand(commands.NewImprovementCmd())
	root.AddCommand(commands.NewWorkItemCmd())

	if err := root.Execute(); err != nil {
		// Silent errors are the best-effort marker; treat as success.
		if commands.IsSilent(err) {
			return
		}
		// Handlers already printed the error to stderr; just propagate the
		// non-zero exit code.
		os.Exit(1)
	}
}
