package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/client"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// NewJoinCmd implements `agentctl join` — the events-side join flow for
// plugin v0.3.0's /join-agent-events skill.
//
// Behaviour summary (see ROADMAP #36 / plan §"agentctl new subcommands"):
//
//  1. Validate required flags; resolve project-slug from --flag, env or local
//     config in that order.
//  2. Resolve the bootstrap admin token from --bootstrap-token (path|env:VAR).
//  3. If per-host token already exists chmod 600 and --rotate is not set,
//     skip the mint step (idempotent).
//  4. Otherwise call POST /v1/admin/agents/{name}/mint-token with the
//     bootstrap admin bearer, write the returned plaintext token to
//     ~/.config/concept-workflow/agent-hub-token (chmod 600).
//  5. Write the env file ~/.config/concept-workflow/agent-events.env to
//     match v0.2.13's shape (export KEY="VALUE").
//  6. Re-export AGENT_HUB_TOKEN_FILE / AGENT_NAME / AGENT_PROJECT_SLUG in
//     the running process, then invoke the register-agent + (optional)
//     smoke sequence as in-process function calls (no shell-out).
//  7. Print a single structured summary line.
//
// All paths are best-effort: only --strict will exit 1 on any single step.
func NewJoinCmd() *cobra.Command {
	var (
		name            string
		alias           string
		role            string
		projectSlug     string
		bootstrapToken  string
		smoke           bool
		rotate          bool
	)

	cmd := &cobra.Command{
		Use:   "join",
		Short: "Bootstrap this host as an agent-events peer (mint token + register + optional smoke)",
		Long: `Bootstrap this host as an agent-events peer.

Reads a bootstrap admin token (--bootstrap-token path-or-env:VARNAME), mints a
per-host bearer via POST /v1/admin/agents/{name}/mint-token, writes the token
+ env file under ~/.config/concept-workflow/, then calls register-agent and
(optionally) a session-start/event-emit/resume-context/session-end smoke
round-trip.

Idempotent: re-running is a no-op unless --rotate is passed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// agent-events.env is loaded by the operator-side caller; we
			// only need the URL for the mint call and the post-mint env
			// vars we'll set in-process below. We deliberately do NOT
			// require AGENT_HUB_TOKEN_FILE yet — the whole point of join
			// is to write that file.
			cfg, err := config.Load(false)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: %v\n", err)
				if strictFlag(cmd) {
					return err
				}
				return nil
			}
			auditor := audit.New(cfg.AuditLog)

			// --name default: agent-operator-mac on Darwin, required elsewhere.
			if name == "" {
				if runtime.GOOS == "darwin" {
					name = "agent-operator-mac"
				} else {
					err := validationError(cmd, auditor, "join", fmt.Errorf("--name is required on %s (no default)", runtime.GOOS))
					if IsSilent(err) {
						return nil
					}
					return err
				}
			}

			// --project-slug: flag > env > error. We don't read the YAML
			// config here because callers ($AGENT_PROJECT_SLUG is set by
			// the same operator config that picks the slug); this avoids
			// dragging a YAML parser into agentctl for one field.
			if projectSlug == "" {
				projectSlug = os.Getenv(config.EnvProjectSlug)
			}
			if projectSlug == "" {
				err := validationError(cmd, auditor, "join", fmt.Errorf("--project-slug is required (or set %s)", config.EnvProjectSlug))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			// --role default: caller-supplied or "frontend".
			if role == "" {
				role = "frontend"
			}

			// --alias: prompt interactively if absent.
			if alias == "" {
				prompted, perr := promptAlias(cmd)
				if perr != nil {
					err := validationError(cmd, auditor, "join", fmt.Errorf("--alias prompt: %w", perr))
					if IsSilent(err) {
						return nil
					}
					return err
				}
				alias = prompted
			}
			if alias == "" {
				err := validationError(cmd, auditor, "join", fmt.Errorf("--alias is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			// Resolve token paths under ~/.config/concept-workflow/.
			home, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: resolve home: %v\n", err)
				if strictFlag(cmd) {
					return err
				}
				return nil
			}
			cfgDir := filepath.Join(home, ".config", "concept-workflow")
			tokenPath := filepath.Join(cfgDir, "agent-hub-token")
			envPath := filepath.Join(cfgDir, "agent-events.env")

			// Step 3: skip mint if token already present chmod 600 and
			// --rotate is not set.
			tokenOK, tokenSkipReason := tokenIsUsable(tokenPath)
			if tokenOK && !rotate {
				fmt.Fprintln(cmd.ErrOrStderr(), "agentctl join: token already present, using existing")
			} else {
				if !tokenOK && tokenSkipReason != "" {
					// Helpful breadcrumb — non-fatal.
					fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: minting fresh token (%s)\n", tokenSkipReason)
				}
				// Step 2: resolve the bootstrap admin token.
				if bootstrapToken == "" {
					err := validationError(cmd, auditor, "join", fmt.Errorf("--bootstrap-token is required when no token file exists yet (path or env:VARNAME)"))
					if IsSilent(err) {
						return nil
					}
					return err
				}
				admin, terr := resolveBootstrapToken(bootstrapToken)
				if terr != nil {
					err := validationError(cmd, auditor, "join", fmt.Errorf("resolve bootstrap token: %w", terr))
					if IsSilent(err) {
						return nil
					}
					return err
				}

				plain, merr := mintHostToken(cmd.Context(), cfg.URL, admin, name)
				if merr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: mint-token: %v\n", merr)
					auditor.Append(audit.Entry{
						Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
						Command:   "join",
						Args:      map[string]any{"name": name, "step": "mint-token"},
						Outcome:   "error",
						Error:     merr.Error(),
						Strict:    strictFlag(cmd),
					})
					if strictFlag(cmd) {
						return merr
					}
					return nil
				}

				if err := writeChmod600(cfgDir, tokenPath, plain); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: write token: %v\n", err)
					if strictFlag(cmd) {
						return err
					}
					return nil
				}
			}

			// Step 6: write the env file (mirrors v0.2.13's shape).
			if err := writeAgentEventsEnv(envPath, cfg.URL, tokenPath, name, projectSlug); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: write env file: %v\n", err)
				if strictFlag(cmd) {
					return err
				}
				return nil
			}

			// Re-export so the in-process register/smoke calls below see
			// the freshly-minted token + slug.
			os.Setenv(config.EnvTokenFile, tokenPath)
			os.Setenv(config.EnvAgentName, name)
			os.Setenv(config.EnvProjectSlug, projectSlug)

			// Step 7: register-agent in-process. We compose the same flow
			// the standalone NewRegisterAgentCmd uses but skip the cobra
			// wrapper so we can keep the join run as a single transaction
			// from the operator's perspective.
			hostKind := hostKindFromGOOS(runtime.GOOS)
			if rerr := runRegisterAgent(cmd.Context(), name, role, hostKind, alias, auditor, cmd, strictFlag(cmd)); rerr != nil {
				if strictFlag(cmd) {
					return rerr
				}
				// Best-effort: log and continue.
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: register-agent: %v; continuing (best-effort)\n", rerr)
			}

			// Step 8: smoke round-trip.
			smokeOutcome := "skipped"
			if smoke {
				smokeOutcome = "ok"
				// Constructive-fix for bug #30: smoke session-id must be
				// unique per run so a re-run doesn't collide with an
				// existing agent_sessions row.
				smokeID := fmt.Sprintf("setup-smoke-%s-%d", name, time.Now().UnixNano())
				if serr := runSmokeRoundTrip(cmd.Context(), smokeID, projectSlug, auditor, cmd, strictFlag(cmd)); serr != nil {
					smokeOutcome = "failed"
					fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: smoke: %v\n", serr)
					if strictFlag(cmd) {
						return serr
					}
				}
			}

			fmt.Fprintf(cmd.ErrOrStderr(),
				"join: agent %s registered with alias %s; token written; smoke %s\n",
				name, alias, smokeOutcome,
			)
			auditor.Append(audit.Entry{
				Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				Command:   "join",
				Args:      map[string]any{"name": name, "alias": alias, "project_slug": projectSlug, "smoke": smokeOutcome},
				Outcome:   "ok",
				Strict:    strictFlag(cmd),
			})
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "agent name (defaults to agent-operator-mac on Darwin)")
	cmd.Flags().StringVar(&alias, "alias", "", "agent display name (prompts if absent)")
	cmd.Flags().StringVar(&role, "role", "", "agent role (defaults to 'frontend')")
	cmd.Flags().StringVar(&projectSlug, "project-slug", "", "project slug (required; defaults to AGENT_PROJECT_SLUG)")
	cmd.Flags().StringVar(&bootstrapToken, "bootstrap-token", "", "admin token: path to chmod-600 file or 'env:VARNAME'")
	cmd.Flags().BoolVar(&smoke, "smoke", false, "run session-start → event emit → resume-context → session-end smoke after registration")
	cmd.Flags().BoolVar(&rotate, "rotate", false, "force fresh mint even if existing token present")

	return cmd
}

// promptAlias prints a one-line prompt to stderr and reads a single line
// from stdin. Trims whitespace. Returns an error only if stdin is closed
// before a newline is read.
func promptAlias(cmd *cobra.Command) (string, error) {
	fmt.Fprint(cmd.ErrOrStderr(), "Enter agent alias (e.g. Splinter / Mikey / Donnie): ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// tokenIsUsable returns (true, "") if the token file exists with mode 0600.
// Otherwise returns (false, reason).
func tokenIsUsable(path string) (bool, string) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "no existing token file"
		}
		return false, fmt.Sprintf("stat token file: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Sprintf("existing token file has insecure perms %o", info.Mode().Perm())
	}
	// Also refuse an empty file — could indicate a half-written write.
	raw, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return false, "existing token file is empty or unreadable"
	}
	return true, ""
}

// resolveBootstrapToken accepts either:
//   - "env:VARNAME"  → read os.Getenv("VARNAME")
//   - any other string → treat as a chmod-600 file path
func resolveBootstrapToken(spec string) (string, error) {
	if strings.HasPrefix(spec, "env:") {
		varName := strings.TrimPrefix(spec, "env:")
		if varName == "" {
			return "", fmt.Errorf("env: prefix needs a variable name")
		}
		v := strings.TrimSpace(os.Getenv(varName))
		if v == "" {
			return "", fmt.Errorf("env var %s is empty or unset", varName)
		}
		return v, nil
	}
	// Treat as path. Enforce chmod 600 so a leaked admin token isn't
	// silently accepted.
	info, err := os.Stat(spec)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", spec, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("file %s has insecure perms %o; run: chmod 600 %s", spec, info.Mode().Perm(), spec)
	}
	raw, err := os.ReadFile(spec)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", spec, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("file %s is empty", spec)
	}
	return tok, nil
}

// mintHostToken POSTs to /v1/admin/agents/{name}/mint-token with the admin
// bearer and returns the plaintext per-host token.
func mintHostToken(ctx context.Context, baseURL, adminToken, name string) (string, error) {
	url := baseURL + "/v1/admin/agents/" + name + "/mint-token"
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Accept", "application/json")

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("mint-token HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode mint-token response: %w; body=%s", err, string(body))
	}
	if out.Token == "" {
		return "", fmt.Errorf("mint-token response missing 'token' field; body=%s", string(body))
	}
	return out.Token, nil
}

// writeChmod600 writes data to path with mode 0600, creating the parent dir
// if needed. Uses an atomic rename so a half-written file never leaks.
func writeChmod600(dir, path, data string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-agentctl-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// best-effort cleanup if the rename below fails
		_ = os.Remove(tmpName)
	}()
	if err := os.Chmod(tmpName, 0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod 600 %s: %w", tmpName, err)
	}
	if _, err := tmp.WriteString(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// writeAgentEventsEnv writes the shell-sourcable env file the plugin's
// /join-agent-events skill expects. Format mirrors plugin v0.2.13's shape:
//
//	export AGENT_HUB_URL="..."
//	export AGENT_HUB_TOKEN_FILE="..."
//	export AGENT_NAME="..."
//	export AGENT_PROJECT_SLUG="..."
//
// v0.3.0 adds AGENT_PROJECT_SLUG so per-VM defaulting Just Works.
func writeAgentEventsEnv(path, url, tokenFile, agentName, projectSlug string) error {
	body := fmt.Sprintf(
		"export AGENT_HUB_URL=%q\n"+
			"export AGENT_HUB_TOKEN_FILE=%q\n"+
			"export AGENT_NAME=%q\n"+
			"export AGENT_PROJECT_SLUG=%q\n",
		url, tokenFile, agentName, projectSlug,
	)
	dir := filepath.Dir(path)
	// envs aren't secret-grade but chmod 600 keeps a single audit surface.
	return writeChmod600(dir, path, body)
}

// hostKindFromGOOS maps runtime.GOOS to the canonical host_kind label.
func hostKindFromGOOS(goos string) string {
	switch goos {
	case "darwin":
		return "mac-host"
	case "linux":
		return "linux-vm"
	default:
		return goos
	}
}

// runRegisterAgent runs the equivalent of `agentctl register-agent` as an
// in-process function call so /join can avoid a sub-process shell-out.
// Uses the freshly-written token file via the process-level env vars
// (already exported by the caller).
func runRegisterAgent(ctx context.Context, name, role, hostKind, alias string, auditor *audit.Writer, cmd *cobra.Command, strict bool) error {
	cfg, err := config.Load(true)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	body := map[string]any{"name": name}
	if role != "" {
		body["role"] = role
	}
	if hostKind != "" {
		body["host_kind"] = hostKind
	}
	if alias != "" {
		body["mattermost_username"] = alias
	}
	cl := client.New(cfg)
	return runCall(ctx, callOpts{
		cmdName: "join.register-agent",
		args:    map[string]any{"name": name, "role": role, "host_kind": hostKind, "alias": alias},
		io:      cmdIO(cmd),
		strict:  strict,
		auditor: auditor,
		renderMutate: func(_ []byte) (string, error) {
			return fmt.Sprintf("join.register-agent: agent %s registered", name), nil
		},
	}, func(ctx context.Context) (int, []byte, error) {
		return cl.Do(ctx, "POST", "/v1/agents/register", body)
	})
}

// runSmokeRoundTrip executes session-start → event emit (test.smoke) →
// resume-context → session-end against the gateway. Uses a unique session
// id supplied by the caller (bug #30 fix).
func runSmokeRoundTrip(ctx context.Context, sessionID, projectSlug string, auditor *audit.Writer, cmd *cobra.Command, strict bool) error {
	cfg, err := config.Load(true)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cl := client.New(cfg)

	// session-start
	if err := runCall(ctx, callOpts{
		cmdName: "join.session-start",
		args:    map[string]any{"claude_session_id": sessionID, "project_slug": projectSlug},
		io:      cmdIO(cmd),
		strict:  strict,
		auditor: auditor,
		renderMutate: func(_ []byte) (string, error) {
			return fmt.Sprintf("join.session-start: session %s started", shortID(sessionID)), nil
		},
	}, func(ctx context.Context) (int, []byte, error) {
		return cl.Do(ctx, "POST", "/v1/sessions/start", map[string]any{
			"claude_session_id": sessionID,
			"project_slug":      projectSlug,
			"start_reason":      "agentctl join --smoke",
		})
	}); err != nil {
		return err
	}

	// event emit (test.smoke)
	if err := runCall(ctx, callOpts{
		cmdName: "join.event-emit",
		args:    map[string]any{"event_type": "test.smoke", "claude_session_id": sessionID},
		io:      cmdIO(cmd),
		strict:  strict,
		auditor: auditor,
		renderMutate: func(_ []byte) (string, error) {
			return "join.event-emit: emitted test.smoke", nil
		},
	}, func(ctx context.Context) (int, []byte, error) {
		return cl.Do(ctx, "POST", "/v1/events", map[string]any{
			"event_type":        "test.smoke",
			"summary":           "agentctl join smoke",
			"project_slug":      projectSlug,
			"claude_session_id": sessionID,
		})
	}); err != nil {
		return err
	}

	// resume-context
	if err := runCall(ctx, callOpts{
		cmdName:    "join.resume-context",
		args:       map[string]any{"claude_session_id": sessionID},
		io:         cmdIO(cmd),
		strict:     strict,
		auditor:    auditor,
		renderRead: renderJSONResponse,
	}, func(ctx context.Context) (int, []byte, error) {
		return cl.Do(ctx, "GET", "/v1/sessions/"+sessionID+"/resume-context", nil)
	}); err != nil {
		return err
	}

	// session-end
	if err := runCall(ctx, callOpts{
		cmdName: "join.session-end",
		args:    map[string]any{"claude_session_id": sessionID, "final_status": "task_completed"},
		io:      cmdIO(cmd),
		strict:  strict,
		auditor: auditor,
		renderMutate: func(_ []byte) (string, error) {
			return fmt.Sprintf("join.session-end: session %s ended", shortID(sessionID)), nil
		},
	}, func(ctx context.Context) (int, []byte, error) {
		return cl.Do(ctx, "POST", "/v1/sessions/end", map[string]any{
			"claude_session_id": sessionID,
			"final_status":      "task_completed",
		})
	}); err != nil {
		return err
	}
	return nil
}
