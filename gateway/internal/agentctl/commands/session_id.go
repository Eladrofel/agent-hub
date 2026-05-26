// session_id.go — shared `claude_session_id` resolution helper (v0.1.11).
//
// The cross-/clear handoff is only usable if every event row carries an
// `agent_session_id`. The gateway resolves that column from the
// `claude_session_id` field in the POST /v1/events body (see
// events.ResolveSessionID). Pre-v0.1.11 only `agentctl checkpoint` and
// `agentctl event emit` accepted the field at all — and `event emit` required
// the flag explicitly while `agentctl improvement emit` had no path to pass
// it. The result (per Dale's 2026-05-23 empirical test): every improvement-
// note event landed with `agent_session_id IS NULL`, so resume-context's
// per-session event query filtered them out → cross-/clear lost every
// captured learning.
//
// The fix is a tiny shared resolver: flag wins, env (`CLAUDE_SESSION_ID`,
// set by Claude Code in tool contexts) is the fallback, empty otherwise.
// Callers that emit events should plumb the returned id into the request
// body and warn-to-stderr (but continue) if both are absent — improvement-
// notes can legitimately be emitted from a one-off operator script with no
// session context, so we don't halt.
package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// claudeSessionIDEnv is the env-var name Claude Code sets in tool contexts.
// Centralised so the spelling matches across resume_context.go + emit paths.
const claudeSessionIDEnv = "CLAUDE_SESSION_ID"

// claudeSessionIDFileEnv overrides the default cache-file path. v0.1.17.
const claudeSessionIDFileEnv = "CLAUDE_SESSION_ID_FILE"

// defaultClaudeSessionIDFile returns the path the concept-workflow plugin's
// SessionStart hook writes (v0.5.7+). agentctl reads it as a third fallback
// when neither the explicit flag nor $CLAUDE_SESSION_ID env is set. This
// closes the gap empirically observed since v0.1.11: Claude Code's Bash
// tool spawns subshells that don't reliably inherit $CLAUDE_SESSION_ID, so
// the env fallback misses every bash-invocation of agentctl in practice.
// Empty return (rare — only when $HOME can't be resolved) disables the
// file fallback.
func defaultClaudeSessionIDFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "concept-workflow", "claude-session-id")
}

// resolveClaudeSessionID returns the effective claude_session_id for an
// emit-style call. Precedence (v0.1.17): explicit --claude-session-id flag
// value → $CLAUDE_SESSION_ID env → cache-file contents (default path
// $HOME/.cache/concept-workflow/claude-session-id, override via
// $CLAUDE_SESSION_ID_FILE) → empty string. Empty is a legitimate outcome
// for one-off operator scripts that run outside a Claude Code session
// entirely; the caller decides whether to warn.
func resolveClaudeSessionID(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(claudeSessionIDEnv); v != "" {
		return v
	}
	path := os.Getenv(claudeSessionIDFileEnv)
	if path == "" {
		path = defaultClaudeSessionIDFile()
	}
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// warnMissingSessionID writes the standard "event will be session-orphaned"
// stderr line. Callers should invoke this only when resolveClaudeSessionID
// returned empty AND the emit path is one where session-tagging is the
// preferred posture (improvement-notes, generic event emit when the operator
// hasn't opted out). Best-effort: we never halt on a missing session id.
func warnMissingSessionID(w io.Writer, cmdLabel string) {
	fmt.Fprintf(w,
		"%s: no %s — event will be session-orphaned (cross-/clear handoff will not surface it)\n",
		cmdLabel, claudeSessionIDEnv)
}
