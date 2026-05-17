package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv unsets every config env var so a test starts from a known state.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvURL, EnvTokenFile, EnvAgentName, EnvProjectSlug, EnvAuditLog} {
		t.Setenv(k, "")
	}
}

func TestLoad_RequiresURL(t *testing.T) {
	clearEnv(t)
	_, err := Load(false)
	if err == nil {
		t.Fatal("expected error when AGENT_HUB_URL unset")
	}
	if !strings.Contains(err.Error(), EnvURL) {
		t.Fatalf("error %v does not mention %s", err, EnvURL)
	}
}

func TestLoad_NoAuth_OnlyNeedsURL(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvURL, "http://example:8787")
	cfg, err := Load(false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.URL != "http://example:8787" {
		t.Fatalf("URL = %q", cfg.URL)
	}
	if cfg.AuditLog == "" {
		t.Fatal("AuditLog should have a default")
	}
}

func TestLoad_Auth_ReadsTokenFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenPath, []byte("  secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvURL, "http://example:8787")
	t.Setenv(EnvTokenFile, tokenPath)
	t.Setenv(EnvAgentName, "agent-1")
	t.Setenv(EnvAuditLog, filepath.Join(dir, "audit.log"))

	cfg, err := Load(true)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.Token != "secret-token" {
		t.Fatalf("Token = %q, want %q (trimmed)", cfg.Token, "secret-token")
	}
	if cfg.AgentName != "agent-1" {
		t.Fatalf("AgentName = %q", cfg.AgentName)
	}
}

func TestLoad_Auth_RejectsMissingTokenFileEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvURL, "http://example:8787")
	t.Setenv(EnvAgentName, "agent-1")
	_, err := Load(true)
	if err == nil || !strings.Contains(err.Error(), EnvTokenFile) {
		t.Fatalf("expected error mentioning %s, got %v", EnvTokenFile, err)
	}
}

func TestLoad_Auth_RejectsMissingAgentName(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	_ = os.WriteFile(tokenPath, []byte("x"), 0o600)
	t.Setenv(EnvURL, "http://example:8787")
	t.Setenv(EnvTokenFile, tokenPath)
	_, err := Load(true)
	if err == nil || !strings.Contains(err.Error(), EnvAgentName) {
		t.Fatalf("expected error mentioning %s, got %v", EnvAgentName, err)
	}
}

func TestLoad_Auth_RejectsEmptyTokenFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	_ = os.WriteFile(tokenPath, []byte("   \n"), 0o600)
	t.Setenv(EnvURL, "http://example:8787")
	t.Setenv(EnvTokenFile, tokenPath)
	t.Setenv(EnvAgentName, "agent-1")
	_, err := Load(true)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected 'empty' error, got %v", err)
	}
}

func TestLoad_AuditLog_DefaultsToHome(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvURL, "http://example:8787")
	cfg, err := Load(false)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "state", "agent-events", "audit.log")
	if cfg.AuditLog != want {
		t.Fatalf("AuditLog = %q, want %q", cfg.AuditLog, want)
	}
}
