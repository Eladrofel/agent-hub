// Package audit writes append-only JSONL records describing each agentctl
// invocation. The log lets an operator reconstruct what an agent attempted
// even when the gateway was unreachable (the central best-effort posture).
//
// The audit writer is best-effort itself: a failure to open or write the
// log emits one stderr line but never propagates to the caller. A failed
// audit must not turn a successful command into a failed one.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry is one row in the audit log. Fields kept to a flat shape so jq
// queries stay short.
type Entry struct {
	Timestamp  string `json:"timestamp"`            // RFC3339Nano
	Command    string `json:"command"`              // e.g. "session-start"
	Args       any    `json:"args,omitempty"`       // sanitised arg snapshot
	Outcome    string `json:"outcome"`              // "ok" | "error"
	HTTPStatus int    `json:"http_status,omitempty"`
	Error      string `json:"error,omitempty"`
	Strict     bool   `json:"strict,omitempty"`
}

// Writer is the audit log handle.
type Writer struct {
	path string
}

// New returns a Writer that targets path. The parent directory is created
// lazily on the first Append call (so a misconfigured path doesn't fail
// init for read-only commands).
func New(path string) *Writer {
	return &Writer{path: path}
}

// Append writes one JSONL line. Errors are reported to stderr but never
// returned — see the package doc.
func (w *Writer) Append(entry Entry) {
	if w == nil || w.path == "" {
		return
	}
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "agentctl audit: mkdir %q: %v\n", filepath.Dir(w.path), err)
		return
	}

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentctl audit: open %q: %v\n", w.path, err)
		return
	}
	defer f.Close()

	raw, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentctl audit: marshal: %v\n", err)
		return
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "agentctl audit: write %q: %v\n", w.path, err)
	}
}
