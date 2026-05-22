package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// NewJoinCodeCmd is the `agentctl join-code` group. v0.1.7 ships one leaf
// — `mint` — invoked on the operator's Mac to mint a signed, single-use
// join-code that a third-party agent VM redeems via `agentctl join --code`.
//
// See HANDOFF.md §Track B and the locked wire contract on
// POST /admin/join-codes / POST /join-codes/redeem.
func NewJoinCodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join-code",
		Short: "Operator-side join-code mint + redeem helpers",
	}
	cmd.AddCommand(newJoinCodeMintCmd())
	return cmd
}

// newJoinCodeMintCmd implements `agentctl join-code mint`. Stays in this file
// (not a separate file) so the wire-contract types are visible to both the
// mint emitter and the redeem consumer in runJoinViaCode.
func newJoinCodeMintCmd() *cobra.Command {
	var (
		agentCanonical       string
		alias                string
		role                 string
		ttl                  time.Duration
		gatewayURL           string
		adminTokenFile       string
		mintAuthorityFile    string
	)

	cmd := &cobra.Command{
		Use:   "mint",
		Short: "Mint a single-use signed join-code for a third-party agent",
		Long: `Mint a single-use, TTL-bounded join-code signed by the gateway. Hand the
returned code to the operator of the agent VM that will redeem it with
'agentctl join --code <code>'.

Requires both --admin-token-file (the gateway admin bearer) and
--mint-authority-token-file (the secondary credential controlling who may
mint codes — see Track A's POST /admin/join-codes contract).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(false)
			if err != nil {
				// config.Load only requires AGENT_HUB_URL; --gateway-url
				// override below handles the "URL came in from a flag" case
				// before we hard-error. Keep going if --gateway-url is set.
				if strings.TrimSpace(gatewayURL) == "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join-code mint: %v\n", err)
					if strictFlag(cmd) {
						return err
					}
					return nil
				}
				cfg = &config.Config{URL: strings.TrimSpace(gatewayURL)}
			}
			auditor := audit.New(cfg.AuditLog)

			// Resolve URL precedence: --gateway-url flag > config (env).
			url := strings.TrimRight(strings.TrimSpace(gatewayURL), "/")
			if url == "" {
				url = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
			}
			if url == "" {
				err := validationError(cmd, auditor, "join-code.mint",
					fmt.Errorf("gateway URL required (set --gateway-url or AGENT_HUB_URL)"))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			if strings.TrimSpace(agentCanonical) == "" {
				err := validationError(cmd, auditor, "join-code.mint",
					fmt.Errorf("--agent is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if strings.TrimSpace(alias) == "" {
				err := validationError(cmd, auditor, "join-code.mint",
					fmt.Errorf("--alias is required"))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			if ttl <= 0 {
				ttl = 24 * time.Hour
			}
			if strings.TrimSpace(role) == "" {
				role = "agent"
			}

			adminToken, terr := readSecretFile(adminTokenFile)
			if terr != nil {
				err := validationError(cmd, auditor, "join-code.mint",
					fmt.Errorf("read --admin-token-file: %w", terr))
				if IsSilent(err) {
					return nil
				}
				return err
			}
			mintAuthToken, terr := readSecretFile(mintAuthorityFile)
			if terr != nil {
				err := validationError(cmd, auditor, "join-code.mint",
					fmt.Errorf("read --mint-authority-token-file: %w", terr))
				if IsSilent(err) {
					return nil
				}
				return err
			}

			resp, merr := mintJoinCode(cmd.Context(), url, adminToken, mintAuthToken, joinCodeMintRequest{
				AgentCanonical: agentCanonical,
				Alias:          alias,
				TTLSeconds:     int64(ttl.Seconds()),
				IntendedRole:   role,
			})
			if merr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join-code mint: %v\n", merr)
				auditor.Append(audit.Entry{
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					Command:   "join-code.mint",
					Args:      map[string]any{"agent": agentCanonical, "alias": alias, "ttl": ttl.String(), "role": role},
					Outcome:   "error",
					Error:     merr.Error(),
					Strict:    strictFlag(cmd),
				})
				if strictFlag(cmd) {
					return merr
				}
				return nil
			}

			// Render the operator-facing block on stdout (so it can be
			// piped/captured) with a copy-paste-ready redeem command.
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Join code (single-use, expires %s):\n", resp.ExpiresAt)
			fmt.Fprintf(out, "  %s\n\n", resp.Code)
			fmt.Fprintf(out, "Hand to operator of %s, run on their VM:\n", resp.AgentCanonical)
			fmt.Fprintf(out, "  agentctl join --code %s --gateway-url %s\n\n", resp.Code, url)
			fmt.Fprintf(out, "(code is single-use — once redeemed, it cannot be reused. TTL: %s.)\n", ttl)

			auditor.Append(audit.Entry{
				Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				Command:   "join-code.mint",
				Args:      map[string]any{"agent": agentCanonical, "alias": alias, "ttl": ttl.String(), "role": role, "expires_at": resp.ExpiresAt},
				Outcome:   "ok",
				Strict:    strictFlag(cmd),
			})
			return nil
		},
	}

	cmd.Flags().StringVar(&agentCanonical, "agent", "", "canonical agent name to bind the code to (e.g. agent-3)")
	cmd.Flags().StringVar(&alias, "alias", "", "agent display alias (e.g. Raph)")
	cmd.Flags().StringVar(&role, "role", "", "intended role (defaults to 'agent')")
	cmd.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "code lifetime (e.g. 24h, 30m)")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "gateway base URL (overrides AGENT_HUB_URL)")
	cmd.Flags().StringVar(&adminTokenFile, "admin-token-file", "", "path to chmod-600 file holding the gateway admin bearer")
	cmd.Flags().StringVar(&mintAuthorityFile, "mint-authority-token-file", "", "path to chmod-600 file holding the mint-authority bearer")

	return cmd
}

// joinCodeMintRequest matches Track A's POST /admin/join-codes body.
type joinCodeMintRequest struct {
	AgentCanonical string `json:"agent_canonical"`
	Alias          string `json:"alias"`
	TTLSeconds     int64  `json:"ttl_seconds"`
	IntendedRole   string `json:"intended_role"`
}

// joinCodeMintResponse matches Track A's 201 response on POST /admin/join-codes.
type joinCodeMintResponse struct {
	Code           string `json:"code"`
	AgentCanonical string `json:"agent_canonical"`
	ExpiresAt      string `json:"expires_at"`
	SingleUse      bool   `json:"single_use"`
}

// joinCodeRedeemRequest matches Track A's POST /join-codes/redeem body.
type joinCodeRedeemRequest struct {
	Code      string `json:"code"`
	Hostname  string `json:"hostname"`
	SSHPubkey string `json:"ssh_pubkey,omitempty"`
}

// joinCodeRedeemResponse matches Track A's 200 response on POST /join-codes/redeem.
type joinCodeRedeemResponse struct {
	AgentCanonical string `json:"agent_canonical"`
	Alias          string `json:"alias"`
	Role           string `json:"role"`
	MintToken      string `json:"mint_token"`
	GatewayURL     string `json:"gateway_url"`
	RegisteredAt   string `json:"registered_at"`
}

// readSecretFile reads a chmod-600 file holding a bearer token and returns
// the trimmed contents. Refuses files with insecure permissions (a leaked
// admin token is leaked regardless of --strict posture).
func readSecretFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf(
			"file %s has insecure perms %o; expected 0600 (run: chmod 600 %s)",
			path, info.Mode().Perm(), path,
		)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("file %s is empty", path)
	}
	return tok, nil
}

// mintJoinCode calls POST <base>/admin/join-codes with the locked wire
// contract: Authorization=admin bearer, X-Mint-Authority=mint-authority
// bearer.
func mintJoinCode(ctx context.Context, base, adminToken, mintAuthToken string, req joinCodeMintRequest) (*joinCodeMintResponse, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", base+"/admin/join-codes", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)
	httpReq.Header.Set("X-Mint-Authority", "Bearer "+mintAuthToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST /admin/join-codes: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mint join-code HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out joinCodeMintResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode mint response: %w; body=%s", err, string(body))
	}
	if out.Code == "" {
		return nil, fmt.Errorf("mint response missing code; body=%s", string(body))
	}
	return &out, nil
}

// redeemJoinCode calls POST <base>/join-codes/redeem. Surfaces friendly
// errors for the four status codes the wire contract defines: 410 expired,
// 409 already-redeemed, 401 sig invalid, 404 not found.
func redeemJoinCode(ctx context.Context, base, code, hostname string) (*joinCodeRedeemResponse, error) {
	req := joinCodeRedeemRequest{Code: code, Hostname: hostname}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal redeem: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", base+"/join-codes/redeem", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST /join-codes/redeem: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out joinCodeRedeemResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("decode redeem response: %w; body=%s", err, string(body))
		}
		if out.MintToken == "" {
			return nil, fmt.Errorf("redeem response missing mint_token; body=%s", string(body))
		}
		return &out, nil
	}

	switch resp.StatusCode {
	case http.StatusGone:
		return nil, fmt.Errorf("join-code expired (HTTP 410); ask the operator to mint a fresh code")
	case http.StatusConflict:
		return nil, fmt.Errorf("join-code already redeemed (HTTP 409); codes are single-use, ask for a new one")
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("join-code signature invalid (HTTP 401); double-check the code was copied intact")
	case http.StatusNotFound:
		return nil, fmt.Errorf("join-code not found (HTTP 404); the gateway has no record of this code")
	default:
		return nil, fmt.Errorf("redeem HTTP %d: %s", resp.StatusCode, string(body))
	}
}

// runJoinViaCode is the --code branch of `agentctl join`. It calls the
// redeem endpoint, writes the returned mint_token + agent-events.env using
// the same on-disk layout the --bootstrap-token branch uses, then invokes
// the same register-agent + (optional) smoke flow as the canonical path.
//
// Idempotency: the redeem endpoint itself is single-use, so re-invoking
// `join --code <same-code>` will hit 409 and abort. That's intentional —
// re-running on the same VM should use the --bootstrap-token branch (the
// token file from the first redeem is still present chmod 600).
func runJoinViaCode(cmd *cobra.Command, cfg *config.Config, auditor *audit.Writer, code, aliasFlag, roleFlag string) error {
	// Hostname is included in the redeem request as a binding hint so the
	// gateway can record where the code was actually redeemed. Best-effort
	// — if the host has no name, send empty string.
	hostname, _ := os.Hostname()

	resp, rerr := redeemJoinCode(cmd.Context(), cfg.URL, strings.TrimSpace(code), hostname)
	if rerr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: redeem code: %v\n", rerr)
		auditor.Append(audit.Entry{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Command:   "join.redeem",
			Args:      map[string]any{"hostname": hostname, "step": "redeem"},
			Outcome:   "error",
			Error:     rerr.Error(),
			Strict:    strictFlag(cmd),
		})
		if strictFlag(cmd) {
			return rerr
		}
		return nil
	}

	// Use the gateway-supplied canonical name + alias + role as the source
	// of truth; CLI overrides only fill blanks (alias is occasionally
	// re-prompted upstream, but the redeem path treats the gateway's value
	// as authoritative — that's the whole point of the signed code).
	name := resp.AgentCanonical
	alias := resp.Alias
	if strings.TrimSpace(alias) == "" {
		alias = strings.TrimSpace(aliasFlag)
	}
	role := resp.Role
	if strings.TrimSpace(role) == "" {
		role = strings.TrimSpace(roleFlag)
	}
	if role == "" {
		role = "agent"
	}
	// project-slug isn't part of the redeem contract; fall back to env or
	// fail. (The operator who minted the code already knows the project,
	// but the wire contract doesn't carry that field.)
	projectSlug := strings.TrimSpace(os.Getenv(config.EnvProjectSlug))
	if projectSlug == "" {
		err := validationError(cmd, auditor, "join.redeem",
			fmt.Errorf("--project-slug or %s is required", config.EnvProjectSlug))
		if IsSilent(err) {
			return nil
		}
		return err
	}

	// Persist gateway URL: prefer the redeem response's value (gateway is
	// the source of truth) over the URL we used to call it, but fall back
	// to the call URL when the response omits it.
	finalURL := strings.TrimRight(strings.TrimSpace(resp.GatewayURL), "/")
	if finalURL == "" {
		finalURL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	}

	// Resolve token paths under ~/.config/concept-workflow/ — same layout
	// as the --bootstrap-token branch.
	home, herr := os.UserHomeDir()
	if herr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: resolve home: %v\n", herr)
		if strictFlag(cmd) {
			return herr
		}
		return nil
	}
	cfgDir := filepath.Join(home, ".config", "concept-workflow")
	tokenPath := filepath.Join(cfgDir, "agent-hub-token")
	envPath := filepath.Join(cfgDir, "agent-events.env")

	if err := writeChmod600(cfgDir, tokenPath, resp.MintToken); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: write token: %v\n", err)
		if strictFlag(cmd) {
			return err
		}
		return nil
	}
	if err := writeAgentEventsEnv(envPath, finalURL, tokenPath, name, projectSlug); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: write env file: %v\n", err)
		if strictFlag(cmd) {
			return err
		}
		return nil
	}

	// Re-export so the in-process register call below sees the freshly-
	// written token + slug + name.
	os.Setenv(config.EnvURL, finalURL)
	os.Setenv(config.EnvTokenFile, tokenPath)
	os.Setenv(config.EnvAgentName, name)
	os.Setenv(config.EnvProjectSlug, projectSlug)

	hostKind := hostKindFromGOOS(runtime.GOOS)
	if regErr := runRegisterAgent(cmd.Context(), name, role, hostKind, alias, auditor, cmd, strictFlag(cmd)); regErr != nil {
		if strictFlag(cmd) {
			return regErr
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "agentctl join: register-agent: %v; continuing (best-effort)\n", regErr)
	}

	fmt.Fprintf(cmd.ErrOrStderr(),
		"join: agent %s registered with alias %s via signed join-code; token written\n",
		name, alias,
	)
	auditor.Append(audit.Entry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Command:   "join.redeem",
		Args:      map[string]any{"name": name, "alias": alias, "project_slug": projectSlug, "via": "code"},
		Outcome:   "ok",
		Strict:    strictFlag(cmd),
	})
	return nil
}
