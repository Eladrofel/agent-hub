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
package sanitiser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Sanitiser holds the compiled pattern set. Zero value is safe but matches
// nothing — useful for tests, not for production.
type Sanitiser struct {
	patterns []compiledPattern
}

type compiledPattern struct {
	source string // original regex string, used as the human-readable match name
	re     *regexp.Regexp
}

// Load reads patterns from path. Returns a Sanitiser with zero patterns iff
// the file exists but is empty/all-comments — operator intent is ambiguous in
// that case so the caller should warn loudly.
func Load(path string) (*Sanitiser, error) {
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
	return &Sanitiser{patterns: patterns}, nil
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

func (s *Sanitiser) scan(text string) *Match {
	for _, p := range s.patterns {
		if p.re.MatchString(text) {
			return &Match{Pattern: p.source}
		}
	}
	return nil
}

// Count returns the number of compiled patterns. Exposed for /health diagnostics.
func (s *Sanitiser) Count() int {
	if s == nil {
		return 0
	}
	return len(s.patterns)
}
