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
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Count(), 2; got != want {
		t.Fatalf("count = %d, want %d", got, want)
	}
}

func TestLoad_InvalidRegexErrors(t *testing.T) {
	path := writePatterns(t, "valid\n[unclosed\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
}

func TestCheck_MatchInSummary(t *testing.T) {
	path := writePatterns(t, `\b10\.\d+\.\d+\.\d+\b`+"\n")
	s, _ := Load(path)
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
	s, _ := Load(path)
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
	s, _ := Load(path)
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
