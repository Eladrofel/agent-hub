package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/commands/comms_backends"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// commsBackendFactory builds the backend by name. Swappable in tests so we
// can drive comms-join without touching a live Mattermost.
var commsBackendFactory = func(name string) (comms_backends.Backend, error) {
	switch name {
	case "mattermost":
		return comms_backends.NewMattermost()
	}
	return nil, fmt.Errorf("backend %q is not registered", name)
}

// NewCommsJoinCmd implements `agentctl comms-join` — the comms-side join
// flow for plugin v0.3.0's /join-agent-comms skill. Only the Mattermost
// backend is implemented in v0.1.6; slack/discord are v0.4 stubs.
func NewCommsJoinCmd() *cobra.Command {
	var (
		backendName    string
		bootstrapPAT   string
		botName        string
		channel        string
		rotate         bool
	)

	cmd := &cobra.Command{
		Use:   "comms-join",
		Short: "Bootstrap this host as a comms peer (provision bot user + mint PAT)",
		Long: `Bootstrap this host as a comms peer.

Reads a bootstrap admin PAT (--bootstrap-pat path-or-env:VARNAME), provisions
the per-VM bot user via the backend's bot API, adds it to the configured
channel, mints a scoped PAT, and writes:

  ~/.config/concept-workflow/mattermost-bot-pat   (chmod 600)
  ~/.config/concept-workflow/concept-chat.env

Idempotent: re-running is a no-op unless --rotate is passed.

Backends:
  mattermost  — full implementation
  none        — no-op (events-only fleets)
  slack       — v0.4 stub; exits 1
  discord     — v0.4 stub; exits 1`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(false)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: %v\n", err)
				if strictFlag(cmd) {
					return err
				}
				return nil
			}
			auditor := audit.New(cfg.AuditLog)

			switch backendName {
			case "":
				err := validationError(cmd, auditor, "comms-join", fmt.Errorf("--backend is required (mattermost|none|slack|discord)"))
				if IsSilent(err) {
					return nil
				}
				return err
			case "none":
				fmt.Fprintln(cmd.ErrOrStderr(), "comms-join: backend=none — no-op")
				return nil
			case "slack", "discord":
				fmt.Fprintf(cmd.ErrOrStderr(), "comms-join: backend=%s not yet implemented (v0.4 stub)\n", backendName)
				return fmt.Errorf("backend %s not implemented", backendName)
			case "mattermost":
				// fall through
			default:
				err := validationError(cmd, auditor, "comms-join", fmt.Errorf("--backend must be mattermost|none|slack|discord, got %q", backendName))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			// Default bot-name from AGENT_NAME if not given.
			if botName == "" {
				agentName := strings.TrimSpace(os.Getenv(config.EnvAgentName))
				if agentName == "" {
					err := validationError(cmd, auditor, "comms-join", fmt.Errorf("--bot-name is required when %s is unset", config.EnvAgentName))
					if IsSilent(err) {
						return nil
					}
					return err
				}
				botName = agentName + "-bot"
			}
			if channel == "" {
				channel = "agent-comms"
			}

			// Resolve token paths under ~/.config/concept-workflow/.
			home, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: resolve home: %v\n", err)
				if strictFlag(cmd) {
					return err
				}
				return nil
			}
			cfgDir := filepath.Join(home, ".config", "concept-workflow")
			patPath := filepath.Join(cfgDir, "mattermost-bot-pat")
			envPath := filepath.Join(cfgDir, "concept-chat.env")

			// Idempotency: skip the PAT mint if existing PAT file is
			// chmod 600 and --rotate is not set.
			patOK, patSkipReason := tokenIsUsable(patPath)

			// Build the backend.
			backend, err := commsBackendFactory(backendName)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: %v\n", err)
				if strictFlag(cmd) {
					return err
				}
				return nil
			}

			// We still need to validate the admin PAT + ensure the bot
			// user exists + ensure channel membership even when --rotate
			// is not set, because we cannot know whether the prior PAT
			// was minted against the same bot user. Cheap idempotent
			// calls.
			admin, terr := resolveBootstrapToken(bootstrapPAT)
			if terr != nil {
				if patOK && !rotate {
					// Allow the no-op path to proceed without an admin
					// PAT — the on-disk PAT file is the source of truth.
					fmt.Fprintln(cmd.ErrOrStderr(), "comms-join: PAT already present, using existing (admin PAT not required)")
					goto WRITE_ENV
				}
				err := validationError(cmd, auditor, "comms-join", fmt.Errorf("resolve bootstrap PAT: %w", terr))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			if err := backend.Validate(admin); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: validate admin PAT: %v\n", err)
				if strictFlag(cmd) {
					return err
				}
				return nil
			}

			{
				botID, berr := backend.EnsureBotUser(admin, botName)
				if berr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: ensure bot user: %v\n", berr)
					if strictFlag(cmd) {
						return berr
					}
					return nil
				}
				if cerr := backend.AddBotToChannel(admin, botID, channel); cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: add bot to channel: %v\n", cerr)
					if strictFlag(cmd) {
						return cerr
					}
					return nil
				}

				if patOK && !rotate {
					fmt.Fprintln(cmd.ErrOrStderr(), "comms-join: PAT already present, using existing")
				} else {
					if !patOK && patSkipReason != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "comms-join: minting fresh PAT (%s)\n", patSkipReason)
					}
					pat, perr := backend.MintPAT(admin, botID)
					if perr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: mint PAT: %v\n", perr)
						if strictFlag(cmd) {
							return perr
						}
						return nil
					}
					if werr := writeChmod600(cfgDir, patPath, pat); werr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: write PAT: %v\n", werr)
						if strictFlag(cmd) {
							return werr
						}
						return nil
					}
				}
			}

		WRITE_ENV:
			if werr := writeConceptChatEnv(envPath, channel, patPath); werr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl comms-join: write env file: %v\n", werr)
				if strictFlag(cmd) {
					return werr
				}
				return nil
			}

			fmt.Fprintf(cmd.ErrOrStderr(),
				"comms-join: backend=%s bot=%s channel=%s; PAT written\n",
				backendName, botName, channel,
			)
			auditor.Append(audit.Entry{
				Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				Command:   "comms-join",
				Args:      map[string]any{"backend": backendName, "bot_name": botName, "channel": channel},
				Outcome:   "ok",
				Strict:    strictFlag(cmd),
			})
			return nil
		},
	}

	cmd.Flags().StringVar(&backendName, "backend", "", "comms backend: mattermost|none|slack|discord")
	cmd.Flags().StringVar(&bootstrapPAT, "bootstrap-pat", "", "admin PAT: path to chmod-600 file or 'env:VARNAME'")
	cmd.Flags().StringVar(&botName, "bot-name", "", "bot account name (defaults to <AGENT_NAME>-bot)")
	cmd.Flags().StringVar(&channel, "channel", "", "channel to join (defaults to 'agent-comms')")
	cmd.Flags().BoolVar(&rotate, "rotate", false, "force fresh PAT even if existing one present")

	return cmd
}

// writeConceptChatEnv writes the shell-sourcable env file the chat-emit
// skill expects:
//
//	export CONCEPT_CHAT_MM_URL="..."
//	export CONCEPT_CHAT_MM_PAT_FILE="..."
//	export CONCEPT_CHAT_MM_CHANNEL="..."
//
// MM_URL is read from the live process env (callers source it from the
// operator config before invoking comms-join).
func writeConceptChatEnv(path, channel, patFile string) error {
	mmURL := strings.TrimSpace(os.Getenv("CONCEPT_CHAT_MM_URL"))
	body := fmt.Sprintf(
		"export CONCEPT_CHAT_MM_URL=%q\n"+
			"export CONCEPT_CHAT_MM_PAT_FILE=%q\n"+
			"export CONCEPT_CHAT_MM_CHANNEL=%q\n",
		mmURL, patFile, channel,
	)
	dir := filepath.Dir(path)
	return writeChmod600(dir, path, body)
}
