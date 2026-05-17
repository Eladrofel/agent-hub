package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppend_CreatesFileAndWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "audit.log")
	w := New(path)

	w.Append(Entry{Command: "session-start", Outcome: "ok", HTTPStatus: 201})
	w.Append(Entry{Command: "event emit", Outcome: "error", Error: "boom", Strict: true})

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	var entries []Entry
	s := bufio.NewScanner(f)
	for s.Scan() {
		var e Entry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			t.Fatalf("decode %q: %v", s.Text(), err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Command != "session-start" || entries[0].Outcome != "ok" {
		t.Fatalf("entry[0] = %+v", entries[0])
	}
	if entries[1].Error != "boom" || !entries[1].Strict {
		t.Fatalf("entry[1] = %+v", entries[1])
	}
}

func TestAppend_AddsTimestampWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	w := New(path)
	w.Append(Entry{Command: "health"})

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "timestamp") {
		t.Fatalf("entry missing timestamp: %q", string(raw))
	}
}

func TestAppend_NilWriterIsNoOp(t *testing.T) {
	var w *Writer
	// Must not panic.
	w.Append(Entry{Command: "x"})
}

func TestAppend_EmptyPathIsNoOp(t *testing.T) {
	w := New("")
	w.Append(Entry{Command: "x"}) // must not panic
}

func TestAppend_AppendsInsteadOfTruncating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	w := New(path)
	w.Append(Entry{Command: "first"})
	w.Append(Entry{Command: "second"})

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "first") || !strings.Contains(string(raw), "second") {
		t.Fatalf("expected both entries, got %q", string(raw))
	}
}

func TestAppend_FilePermissionsAre0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	w := New(path)
	w.Append(Entry{Command: "x"})

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %v, want 0600", st.Mode().Perm())
	}
}
