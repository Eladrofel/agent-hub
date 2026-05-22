package sanitiser

import (
	"os"
	"path/filepath"
	"testing"
)

func writePatterns(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "patterns.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_SkipsBlankAndComments(t *testing.T) {
	path := writePatterns(t, "# comment\n\nfoo\nbar\n")
	s, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Count(), 2; got != want {
		t.Fatalf("count = %d, want %d", got, want)
	}
}

func TestLoad_InvalidRegexErrors(t *testing.T) {
	path := writePatterns(t, "valid\n[unclosed\n")
	if _, err := Load(path, nil); err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
}

func TestCheck_MatchInSummary(t *testing.T) {
	path := writePatterns(t, `\b10\.\d+\.\d+\.\d+\b`+"\n")
	s, _ := Load(path, nil)
	m, err := s.Check("agent at 10.0.5.50 went down", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected match, got nil")
	}
	if m.MatchedField != "summary" {
		t.Fatalf("MatchedField = %q, want summary", m.MatchedField)
	}
}

func TestCheck_MatchInPayload(t *testing.T) {
	path := writePatterns(t, "forgejo\n")
	s, _ := Load(path, nil)
	payload := map[string]any{
		"ref": map[string]string{"url": "http://forgejo.example/foo"},
	}
	m, err := s.Check("clean summary", payload)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m.MatchedField != "payload" {
		t.Fatalf("expected payload match, got %+v", m)
	}
}

func TestCheck_NoMatchReturnsNil(t *testing.T) {
	path := writePatterns(t, "neverappears\n")
	s, _ := Load(path, nil)
	m, err := s.Check("clean", map[string]string{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatalf("expected nil match, got %+v", m)
	}
}

func TestCheck_NilSanitiserIsNoop(t *testing.T) {
	var s *Sanitiser
	m, err := s.Check("anything 10.0.0.1", nil)
	if err != nil || m != nil {
		t.Fatalf("nil sanitiser should be no-op, got match=%v err=%v", m, err)
	}
}

// v0.1.3 task #29: exempt-host suppresses the match.
func TestCheck_ExemptHostSuppressesMatch(t *testing.T) {
	path := writePatterns(t, `\b10\.\d+\.\d+\.\d+\b`+"\n")
	s, err := Load(path, []string{"10.0.5.38"})
	if err != nil {
		t.Fatal(err)
	}
	// The matched substring "10.0.5.38" contains the exempt → suppressed.
	m, err := s.Check("gateway at http://10.0.5.38:8787/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatalf("expected no match (exempt), got %+v", m)
	}
}

// Exempt host doesn't accidentally also exempt unrelated IP addresses.
func TestCheck_ExemptHostOnlySuppressesItself(t *testing.T) {
	path := writePatterns(t, `\b10\.\d+\.\d+\.\d+\b`+"\n")
	s, _ := Load(path, []string{"10.0.5.38"})
	m, err := s.Check("rogue leak: 10.0.5.50", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected match on rogue IP, got nil")
	}
}

// Whitespace + empty entries in the exempt list are ignored (so an unset
// env var that splits to [""] doesn't exempt everything).
func TestLoad_EmptyExemptEntriesAreIgnored(t *testing.T) {
	path := writePatterns(t, `\b10\.\d+\.\d+\.\d+\b`+"\n")
	s, _ := Load(path, []string{"", "  "})
	m, err := s.Check("rogue 10.0.5.50", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("empty exempt entries must not exempt anything")
	}
}

// #46 — Mattermost post_id field is a structural identifier and bypasses
// pattern matching even if it happens to match a §2.1 regex.
func TestCheckMattermost_StructuralFieldBypassesMatch(t *testing.T) {
	// Pattern: any 26-char base32 sequence (mimics MM-id shape closely
	// enough that the field's value would normally trip it).
	path := writePatterns(t, `[a-z0-9]{26}\b`+"\n")
	s, _ := Load(path, nil)

	payload := map[string]any{
		"post_id":    "abcdefghijklmnopqrstuvwxyz",
		"channel_id": "1234567890abcdefghijklmnop",
		"user_id":    "zyxwvutsrqponmlkjihgfedcba",
		"text":       "clean message body",
	}
	m, err := s.CheckMattermost("clean summary", payload)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatalf("expected no match (MM ids in structural fields), got %+v", m)
	}
}

// #46 — Free-form text field still gets full scrutiny; a real PII-shaped
// match in the text IS redacted.
func TestCheckMattermost_FreeFormFieldStillScrutinised(t *testing.T) {
	path := writePatterns(t, `\b10\.\d+\.\d+\.\d+\b`+"\n")
	s, _ := Load(path, nil)

	payload := map[string]any{
		"post_id": "abcdefghijklmnopqrstuvwxyz",
		"text":    "the box at 10.0.5.50 is rebooting",
	}
	m, err := s.CheckMattermost("", payload)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected match on free-form text field, got nil")
	}
	if m.MatchedField != "payload" {
		t.Fatalf("matched_field = %q, want payload", m.MatchedField)
	}
}

// #46 — A string that LOOKS like an MM id but is embedded inside the
// free-form `text` field still gets scrutinised. The exemption is
// field-name-based and MUST NOT be triggered by content shape.
func TestCheckMattermost_MMIDShapeInsideTextStillScrutinised(t *testing.T) {
	path := writePatterns(t, `[a-z0-9]{26}\b`+"\n")
	s, _ := Load(path, nil)

	payload := map[string]any{
		"post_id": "abcdefghijklmnopqrstuvwxyz",
		"text":    "look up post abcdefghijklmnopqrstuvwxyz it's relevant",
	}
	m, err := s.CheckMattermost("", payload)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected match: MM-id-shape inside text field is not field-exempt")
	}
}

// #46 — The generic Check entry point still scrutinises EVERY field; the
// MM-aware bypass is opt-in via CheckMattermost only.
func TestCheck_DoesNotBypassStructuralFields(t *testing.T) {
	path := writePatterns(t, `[a-z0-9]{26}\b`+"\n")
	s, _ := Load(path, nil)

	payload := map[string]any{
		"post_id": "abcdefghijklmnopqrstuvwxyz",
	}
	m, err := s.Check("", payload)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("Check (generic) must NOT bypass — MM bypass is opt-in via CheckMattermost")
	}
}

// If a pattern produces multiple matches and only some are exempt, the
// non-exempt match still fires the sanitiser.
func TestCheck_MixedExemptAndNonExemptFires(t *testing.T) {
	path := writePatterns(t, `\b10\.\d+\.\d+\.\d+\b`+"\n")
	s, _ := Load(path, []string{"10.0.5.38"})
	m, err := s.Check("trusted 10.0.5.38 and leak 10.0.5.50", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected match (one non-exempt IP present), got nil")
	}
}
