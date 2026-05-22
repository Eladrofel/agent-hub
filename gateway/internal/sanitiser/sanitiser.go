// Package sanitiser implements the gateway-side §2.1 leak-pattern check.
// Every POST /v1/events runs Check over the event's summary + payload.
// A match HTTP-422s the request; the server then writes a payload-free
// sanitiser.blocked audit event so the operator can see what tripped the
// filter without re-storing the offending content.
//
// Pattern source: a text file (default /etc/agent-hub/sanitiser-patterns.txt
// in-container; override via SANITISER_PATTERNS_FILE). One Go RE2 regex per
// non-blank, non-comment line. Anchors are NOT auto-added; operators write
// ^/$ explicitly if they want them.
//
// Pattern file format intentionally matches the agent-side hook layer's
// references/sanitiser-patterns.example.txt so the two layers stay in sync
// (a leak that escapes the hook trips the gateway and vice versa).
//
// # Exempt hosts (v0.1.3, task #29)
//
// The §2.1 pattern set includes a permissive `\b10\.\d+\.\d+\.\d+\b` rule
// that catches private-range IPv4 leaks. Unfortunately that also catches
// the operator's own agent-hub gateway URL when it appears legitimately
// (e.g., `AGENT_HUB_URL=http://10.0.5.38:8787` in a session-start payload).
// To prevent the sanitiser from blocking its own gateway, callers may pass
// an exempt-hosts list at Load(). When a pattern matches a substring that
// CONTAINS any exempt string, the match is suppressed and scanning
// continues to the next pattern. Configure via the gateway's
// SANITISER_EXEMPT_HOSTS env var (comma-separated).
package sanitiser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Sanitiser holds the compiled pattern set + the configured exempt-hosts
// list. Zero value is safe but matches nothing — useful for tests, not for
// production.
type Sanitiser struct {
	patterns    []compiledPattern
	exemptHosts []string
}

type compiledPattern struct {
	source string // original regex string, used as the human-readable match name
	re     *regexp.Regexp
}

// Load reads patterns from path. Returns a Sanitiser with zero patterns iff
// the file exists but is empty/all-comments — operator intent is ambiguous in
// that case so the caller should warn loudly.
//
// exemptHosts: if any of these substrings appears within a matched portion of
// the scanned text, the match is suppressed. Pass nil/empty for "no
// exemptions" (default §2.1 strictness). Whitespace-trimmed; empty strings
// dropped.
func Load(path string, exemptHosts []string) (*Sanitiser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open sanitiser patterns: %w", err)
	}
	defer f.Close()

	var patterns []compiledPattern
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("sanitiser patterns line %d: invalid regex %q: %w", lineNo, line, err)
		}
		patterns = append(patterns, compiledPattern{source: line, re: re})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read sanitiser patterns: %w", err)
	}

	// Normalise the exempt list — trim + drop empties so an unset env var
	// (which Strings.Split yields as [""]) doesn't accidentally exempt
	// everything by containing-the-empty-string-in-every-match.
	var exempt []string
	for _, h := range exemptHosts {
		h = strings.TrimSpace(h)
		if h != "" {
			exempt = append(exempt, h)
		}
	}

	return &Sanitiser{patterns: patterns, exemptHosts: exempt}, nil
}

// Match is the result of a sanitiser hit. Pattern is the original regex
// string (safe to include in operator-visible error responses); MatchedField
// identifies which input field tripped it. The matched substring itself is
// NEVER returned — including it would defeat the purpose of the sanitiser.
type Match struct {
	Pattern      string
	MatchedField string
}

// Check scans summary and payload for any pattern match. Returns nil if
// clean. payload is JSON-marshalled before scanning so nested map values are
// covered (regex against a stringified jsonb blob is good enough at this
// volume; a deep walker is over-engineering for v0.1.0).
func (s *Sanitiser) Check(summary string, payload any) (*Match, error) {
	if s == nil || len(s.patterns) == 0 {
		return nil, nil
	}

	if summary != "" {
		if hit := s.scan(summary); hit != nil {
			hit.MatchedField = "summary"
			return hit, nil
		}
	}

	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload for scan: %w", err)
		}
		if hit := s.scan(string(raw)); hit != nil {
			hit.MatchedField = "payload"
			return hit, nil
		}
	}
	return nil, nil
}

// mattermostStructuralFields lists payload field paths whose values are
// known-safe structural identifiers from Mattermost (post ids, user ids,
// channel ids, team ids, etc.). They are 26-char base32-alphabet strings
// that frequently trip §2.1 patterns despite being content-free.
//
// IMPORTANT: this is a STRUCTURAL exemption, not a content one. The
// sanitiser still scrutinises free-form fields (text, message) — even if a
// substring inside that text happens to look like a Mattermost id, the
// scrutiny stands. Only fields named in this set bypass pattern matching.
//
// Closes #46 (v0.1.7).
var mattermostStructuralFields = map[string]bool{
	"post_id":     true,
	"user_id":     true,
	"team_id":     true,
	"channel_id":  true,
	"file_ids":    true,
	"root_id":     true,
	"parent_id":   true,
	"trigger_id":  true,
}

// CheckMattermost scans an MM webhook payload with structural-field
// awareness. Unlike Check, it walks the payload as a map[string]any and
// skips any field whose key is in mattermostStructuralFields. The summary +
// free-form fields (text, message, props as a stringified blob) are still
// scanned normally.
//
// Use this entry point in code paths that handle MM-shaped payloads (e.g.,
// the inbox-webhook receiver or future MM-relay events) so structural ids
// don't cause spurious sanitiser.blocked events. Callers handling generic
// agent-emitted payloads should keep using Check.
func (s *Sanitiser) CheckMattermost(summary string, payload map[string]any) (*Match, error) {
	if s == nil || len(s.patterns) == 0 {
		return nil, nil
	}

	if summary != "" {
		if hit := s.scan(summary); hit != nil {
			hit.MatchedField = "summary"
			return hit, nil
		}
	}

	if payload == nil {
		return nil, nil
	}

	// Build a filtered shallow copy that drops known-safe structural fields.
	// Field-name match is on the top-level key only — nested objects still
	// get scrutinised in full, because deeply-nested keys are not part of
	// MM's well-known structural set and could carry attacker-influenced
	// content.
	filtered := make(map[string]any, len(payload))
	for k, v := range payload {
		if mattermostStructuralFields[k] {
			continue
		}
		filtered[k] = v
	}

	raw, err := json.Marshal(filtered)
	if err != nil {
		return nil, fmt.Errorf("marshal payload for mm scan: %w", err)
	}
	if hit := s.scan(string(raw)); hit != nil {
		hit.MatchedField = "payload"
		return hit, nil
	}
	return nil, nil
}

// scan finds the first non-exempt pattern match. A "match" is the
// regex-extracted substring; if that substring contains any of the
// configured exempt-hosts, the match is suppressed and we keep scanning the
// remaining patterns. (Within a single pattern we only inspect the first
// FindString — if multiple matches exist for one pattern, the first is
// representative; if they're all exempt we move to the next pattern.)
func (s *Sanitiser) scan(text string) *Match {
	for _, p := range s.patterns {
		if !p.re.MatchString(text) {
			continue
		}
		// FindAllString gives every match for this pattern; if at least one
		// is NOT in the exempt list, the pattern fires. If every match is
		// covered by an exempt, suppress and continue to the next pattern.
		matches := p.re.FindAllString(text, -1)
		if s.allExempt(matches) {
			continue
		}
		return &Match{Pattern: p.source}
	}
	return nil
}

// allExempt returns true iff every supplied match string contains at least
// one of the configured exempt-host substrings. Empty input returns false
// (no matches → nothing to exempt → caller should treat as not-exempt;
// scan() never reaches here in that case).
func (s *Sanitiser) allExempt(matches []string) bool {
	if len(matches) == 0 || len(s.exemptHosts) == 0 {
		return false
	}
	for _, m := range matches {
		exempt := false
		for _, host := range s.exemptHosts {
			if strings.Contains(m, host) {
				exempt = true
				break
			}
		}
		if !exempt {
			return false
		}
	}
	return true
}

// Count returns the number of compiled patterns. Exposed for /health diagnostics.
func (s *Sanitiser) Count() int {
	if s == nil {
		return 0
	}
	return len(s.patterns)
}
