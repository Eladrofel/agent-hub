// Package config loads agentctl's runtime configuration from environment
// variables. All variables are read at command-execution time (not init), so
// tests can set them via t.Setenv without process restart.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Env names — exported so callers (and tests) can reference them.
const (
	EnvURL         = "AGENT_HUB_URL"
	EnvTokenFile   = "AGENT_HUB_TOKEN_FILE"
	EnvAgentName   = "AGENT_NAME"
	EnvProjectSlug = "AGENT_PROJECT_SLUG"
	EnvAuditLog    = "AGENT_HUB_AUDIT_LOG"
)

// Config holds the resolved runtime configuration for one agentctl invocation.
// Token is the plaintext bearer (already read from TokenFile + trimmed).
type Config struct {
	URL         string
	TokenFile   string
	Token       string
	AgentName   string
	ProjectSlug string
	AuditLog    string
}

// Load reads + validates the env. requireAuth=false skips the token-file and
// agent-name requirements (used by `agentctl health`, which is no-auth).
func Load(requireAuth bool) (*Config, error) {
	cfg := &Config{
		URL:         strings.TrimSpace(os.Getenv(EnvURL)),
		TokenFile:   strings.TrimSpace(os.Getenv(EnvTokenFile)),
		AgentName:   strings.TrimSpace(os.Getenv(EnvAgentName)),
		ProjectSlug: strings.TrimSpace(os.Getenv(EnvProjectSlug)),
		AuditLog:    strings.TrimSpace(os.Getenv(EnvAuditLog)),
	}

	if cfg.URL == "" {
		return nil, fmt.Errorf("env %s is required", EnvURL)
	}

	if cfg.AuditLog == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("default audit log: resolve $HOME: %w", err)
		}
		cfg.AuditLog = filepath.Join(home, ".local", "state", "agent-events", "audit.log")
	}

	if !requireAuth {
		return cfg, nil
	}

	if cfg.TokenFile == "" {
		return nil, fmt.Errorf("env %s is required", EnvTokenFile)
	}
	if cfg.AgentName == "" {
		return nil, fmt.Errorf("env %s is required", EnvAgentName)
	}

	raw, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token file %q: %w", cfg.TokenFile, err)
	}
	// Refuse to load a world/group-readable bearer file. Overriding the
	// reviewer's "warn or refuse-in-strict" suggestion with refuse-always: a
	// leaked bearer is a leaked bearer regardless of --strict posture, and
	// agent VMs will eventually be multi-tenant. Operators get a paste-
	// ready chmod hint in the error message.
	info, statErr := os.Stat(cfg.TokenFile)
	if statErr != nil {
		return nil, fmt.Errorf("stat token file %q: %w", cfg.TokenFile, statErr)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf(
			"token file %q has insecure permissions %o; expected 0600 (run: chmod 600 %s)",
			cfg.TokenFile, info.Mode().Perm(), cfg.TokenFile,
		)
	}
	cfg.Token = strings.TrimSpace(string(raw))
	if cfg.Token == "" {
		return nil, fmt.Errorf("token file %q is empty", cfg.TokenFile)
	}

	return cfg, nil
}
