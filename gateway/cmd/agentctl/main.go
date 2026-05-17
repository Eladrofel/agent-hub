// agentctl is the CLI invoked by VM agents and the operator's Mac to talk to
// the agent-hub gateway over HTTP. Distributed as a single static binary
// cross-compiled for darwin-arm64 (operator Mac) and linux-amd64 (agent VMs).
//
// Subcommands cover the lifecycle:
//
//   agentctl register-agent --name <name> --role <role> [--host-kind <kind>]
//   agentctl session-start --claude-session-id <id> [...]
//   agentctl session-end --claude-session-id <id>
//   agentctl event emit --type <type> --summary <s> [--json <payload>]
//   agentctl checkpoint --task <key> --summary <s> --next <next> [--next <next>]
//   agentctl resume-context [--claude-session-id <id>] [--task <key>]
//   agentctl handoff create|accept|complete [...]
//   agentctl inbox poll [--since <ts>]
//   agentctl health
//
// Configuration (env):
//   AGENT_HUB_URL         — gateway base URL (e.g., http://10.0.5.50:8787)
//   AGENT_HUB_TOKEN_FILE  — path to the per-host token file (chmod 600 on Mac;
//                            systemd-creds-decrypted file on Linux VMs)
//   AGENT_NAME            — this agent's identity (e.g., "agent-1", "agent-operator-mac")
//   AGENT_PROJECT_SLUG    — the project this agent is working on
//
// Best-effort posture: any subcommand that emits an event logs locally on
// failure and continues. Hard-fail callers (e.g., the orchestrator's
// review-posted step) check the exit code; non-fail callers ignore it.
//
// v0.1.0 ships the subcommand routing. Endpoints are stubbed; flesh out in
// v0.1.x patches.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	// agentctl wire-up to the gateway endpoints ships in v0.1.0; the binary
	// version tracks the agent-hub release line.
	version = "0.1.0-dev"
	commit  = "unknown"
)

func main() {
	root := &cobra.Command{
		Use:     "agentctl",
		Short:   "CLI for the agent-events system",
		Version: fmt.Sprintf("%s (commit %s)", version, commit),
	}

	root.AddCommand(&cobra.Command{
		Use:   "register-agent",
		Short: "Register or upsert this agent in the hub",
		RunE:  notImpl,
	})

	root.AddCommand(&cobra.Command{
		Use:   "session-start",
		Short: "Start a new Claude session record",
		RunE:  notImpl,
	})

	root.AddCommand(&cobra.Command{
		Use:   "session-end",
		Short: "End the current Claude session",
		RunE:  notImpl,
	})

	eventCmd := &cobra.Command{Use: "event", Short: "Event subcommands"}
	eventCmd.AddCommand(&cobra.Command{
		Use:   "emit",
		Short: "Emit one event (best-effort; exit 0 on local-log fallback)",
		RunE:  notImpl,
	})
	root.AddCommand(eventCmd)

	root.AddCommand(&cobra.Command{
		Use:   "checkpoint",
		Short: "Write a session checkpoint",
		RunE:  notImpl,
	})

	root.AddCommand(&cobra.Command{
		Use:   "resume-context",
		Short: "Fetch the resume packet for this agent/session",
		RunE:  notImpl,
	})

	handoffCmd := &cobra.Command{Use: "handoff", Short: "Handoff subcommands"}
	for _, sub := range []string{"create", "accept", "complete"} {
		s := sub
		handoffCmd.AddCommand(&cobra.Command{
			Use:   s,
			Short: fmt.Sprintf("%s a handoff", s),
			RunE:  notImpl,
		})
	}
	root.AddCommand(handoffCmd)

	inboxCmd := &cobra.Command{Use: "inbox", Short: "Mattermost inbox subcommands"}
	inboxCmd.AddCommand(&cobra.Command{
		Use:   "poll",
		Short: "Poll inbox for new operator messages",
		RunE:  notImpl,
	})
	root.AddCommand(inboxCmd)

	root.AddCommand(&cobra.Command{
		Use:   "health",
		Short: "Report gateway connectivity + this agent's token status",
		RunE:  notImpl,
	})

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func notImpl(cmd *cobra.Command, args []string) error {
	return fmt.Errorf("%s: not implemented; v0.1.0 stub", cmd.Name())
}
