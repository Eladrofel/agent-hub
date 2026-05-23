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
)

// claudeSessionIDEnv is the env-var name Claude Code sets in tool contexts.
// Centralised so the spelling matches across resume_context.go + emit paths.
const claudeSessionIDEnv = "CLAUDE_SESSION_ID"

// resolveClaudeSessionID returns the effective claude_session_id for an
// emit-style call. Precedence: explicit --claude-session-id flag value, then
// $CLAUDE_SESSION_ID, then empty string. Empty is a legitimate outcome for
// non-session callers (one-off operator scripts); the caller decides whether
// to warn.
func resolveClaudeSessionID(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(claudeSessionIDEnv)
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
