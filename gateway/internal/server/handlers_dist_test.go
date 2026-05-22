package server

// Pure-unit tests for /dist/* binary serving. No Postgres needed; these
// run on every `go test ./...` invocation.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newDistTestApp(t *testing.T, distDir string) *App {
	t.Helper()
	return &App{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		DistDir: distDir,
	}
}

func TestHandleDist_DisabledReturns503(t *testing.T) {
	app := newDistTestApp(t, "") // empty DistDir
	h := app.handleDistAgentctl("agentctl-linux-amd64")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dist/agentctl-linux-amd64", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var resp errorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "dist_disabled" {
		t.Errorf("error = %q, want dist_disabled", resp.Error)
	}
}

func TestHandleDist_MissingFileReturns404(t *testing.T) {
	dir := t.TempDir()
	app := newDistTestApp(t, dir)
	h := app.handleDistAgentctl("agentctl-linux-amd64")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dist/agentctl-linux-amd64", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func TestHandleDist_HappyPath(t *testing.T) {
	dir := t.TempDir()
	body := []byte("#!/bin/sh\necho fake-agentctl\n")
	full := filepath.Join(dir, "agentctl-linux-amd64")
	if err := os.WriteFile(full, body, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	app := newDistTestApp(t, dir)
	h := app.handleDistAgentctl("agentctl-linux-amd64")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dist/agentctl-linux-amd64", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content-type = %q, want octet-stream", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "filename=agentctl") {
		t.Errorf("content-disposition = %q, missing filename=agentctl", got)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("ETag header missing")
	}
	if !strings.HasPrefix(w.Body.String(), "#!/bin/sh") {
		t.Errorf("body prefix unexpected: %q", w.Body.String())
	}
}

func TestHandleDist_HeadRequestReturnsHeadersWithoutBody(t *testing.T) {
	// HEAD probe (caching proxies, link-checkers) must succeed with the
	// same headers GET produces and an empty body. http.ServeContent
	// strips the body automatically when r.Method == "HEAD".
	dir := t.TempDir()
	body := []byte("#!/bin/sh\necho fake-agentctl\n")
	full := filepath.Join(dir, "agentctl-linux-amd64")
	if err := os.WriteFile(full, body, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	app := newDistTestApp(t, dir)
	h := app.handleDistAgentctl("agentctl-linux-amd64")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/dist/agentctl-linux-amd64", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content-type = %q, want octet-stream", got)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("ETag header missing on HEAD response")
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD response body length = %d, want 0", w.Body.Len())
	}
}

func TestHandleDist_DirectoryReturns500(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "agentctl-linux-amd64"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	app := newDistTestApp(t, dir)
	h := app.handleDistAgentctl("agentctl-linux-amd64")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dist/agentctl-linux-amd64", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", w.Code)
	}
}
