// Integration tests for the project upsert / list / get handlers added in
// v0.1.2. These require AGENT_HUB_TEST_DATABASE_URL (skip otherwise), same
// convention as the rest of internal/server.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Eladrofel/agent-hub/gateway/internal/projects"
)

// =============================================================================
// POST /v1/projects
// =============================================================================

func TestProjectUpsert_HappyPath(t *testing.T) {
	env := newTestEnv(t, "")

	w := env.request("POST", "/v1/projects", env.agentToken, map[string]any{
		"slug": "secureup",
		"name": "Secureup",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp projects.Project
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("id empty")
	}
	if resp.Slug != "secureup" || resp.Name != "Secureup" {
		t.Fatalf("got slug=%q name=%q", resp.Slug, resp.Name)
	}
	// Default branch should default to 'main'.
	if resp.DefaultBranch == nil || *resp.DefaultBranch != "main" {
		t.Fatalf("default_branch = %v, want 'main'", resp.DefaultBranch)
	}

	// Verify the row landed.
	var gotSlug string
	err := env.store.Pool.QueryRow(env.ctx,
		`SELECT slug FROM projects WHERE id = $1`, resp.ID).Scan(&gotSlug)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if gotSlug != "secureup" {
		t.Fatalf("db slug = %q", gotSlug)
	}
}

func TestProjectUpsert_WithAllFields(t *testing.T) {
	env := newTestEnv(t, "")

	forge := "ssh://git@forge:2222/x/workspace.git"
	branch := "develop"
	outbox := "agent-events"
	inbox := "agent-inbox"

	w := env.request("POST", "/v1/projects", env.agentToken, map[string]any{
		"slug":                       "alpha",
		"name":                       "Alpha",
		"forge_url":                  forge,
		"default_branch":             branch,
		"mattermost_outbox_channel":  outbox,
		"mattermost_inbox_channel":   inbox,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp projects.Project
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ForgeURL == nil || *resp.ForgeURL != forge {
		t.Fatalf("forge_url = %v", resp.ForgeURL)
	}
	if resp.DefaultBranch == nil || *resp.DefaultBranch != branch {
		t.Fatalf("default_branch = %v", resp.DefaultBranch)
	}
	if resp.MattermostOutboxChannel == nil || *resp.MattermostOutboxChannel != outbox {
		t.Fatalf("outbox = %v", resp.MattermostOutboxChannel)
	}
	if resp.MattermostInboxChannel == nil || *resp.MattermostInboxChannel != inbox {
		t.Fatalf("inbox = %v", resp.MattermostInboxChannel)
	}
}

func TestProjectUpsert_RejectsMissingAuth(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/projects", "", map[string]any{"slug": "x", "name": "X"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestProjectUpsert_RejectsMissingSlug(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/projects", env.agentToken, map[string]any{"name": "X"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp errorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "slug_required" {
		t.Fatalf("error = %q, want 'slug_required'", resp.Error)
	}
}

func TestProjectUpsert_RejectsMissingName(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("POST", "/v1/projects", env.agentToken, map[string]any{"slug": "x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp errorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "name_required" {
		t.Fatalf("error = %q, want 'name_required'", resp.Error)
	}
}

func TestProjectUpsert_IdempotentOnConflict(t *testing.T) {
	env := newTestEnv(t, "")

	// First call.
	w := env.request("POST", "/v1/projects", env.agentToken, map[string]any{
		"slug": "beta", "name": "Beta",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("first call: status = %d; body=%s", w.Code, w.Body.String())
	}
	var first projects.Project
	_ = json.Unmarshal(w.Body.Bytes(), &first)

	// Second call with same slug but updated name.
	w = env.request("POST", "/v1/projects", env.agentToken, map[string]any{
		"slug": "beta", "name": "Beta Updated",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("second call: status = %d; body=%s", w.Code, w.Body.String())
	}
	var second projects.Project
	_ = json.Unmarshal(w.Body.Bytes(), &second)

	if first.ID != second.ID {
		t.Fatalf("id changed across upserts: %s -> %s (should be same row)", first.ID, second.ID)
	}
	if second.Name != "Beta Updated" {
		t.Fatalf("name not updated: %q", second.Name)
	}

	// Verify only one row exists.
	var n int
	if err := env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*) FROM projects WHERE slug = 'beta'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d rows with slug=beta, want 1", n)
	}
}

// =============================================================================
// GET /v1/projects
// =============================================================================

func TestProjectList_ReturnsEmptyWhenNone(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("GET", "/v1/projects", env.agentToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp projectListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Projects) != 0 {
		t.Fatalf("got %d projects, want 0", len(resp.Projects))
	}
}

func TestProjectList_ReturnsAllOrderedBySlug(t *testing.T) {
	env := newTestEnv(t, "")
	// Seed three out of order.
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		_, err := env.store.Pool.Exec(context.Background(),
			`INSERT INTO projects (slug, name) VALUES ($1, $1)`, name)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	w := env.request("GET", "/v1/projects", env.agentToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp projectListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Projects) != 3 {
		t.Fatalf("got %d projects, want 3", len(resp.Projects))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, p := range resp.Projects {
		if p.Slug != want[i] {
			t.Fatalf("idx %d: got slug %q, want %q", i, p.Slug, want[i])
		}
	}
}

// =============================================================================
// GET /v1/projects/{slug}
// =============================================================================

func TestProjectGet_HappyPath(t *testing.T) {
	env := newTestEnv(t, "")
	_, err := env.store.Pool.Exec(context.Background(),
		`INSERT INTO projects (slug, name) VALUES ('gamma', 'Gamma')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := env.request("GET", "/v1/projects/gamma", env.agentToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp projects.Project
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Slug != "gamma" || resp.Name != "Gamma" {
		t.Fatalf("slug=%q name=%q", resp.Slug, resp.Name)
	}
}

func TestProjectGet_Returns404OnMissing(t *testing.T) {
	env := newTestEnv(t, "")
	w := env.request("GET", "/v1/projects/nonexistent", env.agentToken, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var resp errorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "project_not_found" {
		t.Fatalf("error = %q, want 'project_not_found'", resp.Error)
	}
}
