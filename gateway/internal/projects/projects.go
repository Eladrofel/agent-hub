// Package projects owns the projects table. One row per consuming workspace
// (e.g., secureup-concepts). Events, sessions, and tasks scope to a project
// via project_slug; this DAO is the lookup + provisioning layer the gateway's
// /v1/projects handlers wrap so /setup-agent-events can register the
// workspace at provisioning time without needing direct SQL access.
package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Project is the public-facing shape.
type Project struct {
	ID                       string  `json:"id"`
	Slug                     string  `json:"slug"`
	Name                     string  `json:"name"`
	ForgeURL                 *string `json:"forge_url,omitempty"`
	DefaultBranch            *string `json:"default_branch,omitempty"`
	MattermostOutboxChannel  *string `json:"mattermost_outbox_channel,omitempty"`
	MattermostInboxChannel   *string `json:"mattermost_inbox_channel,omitempty"`
	CreatedAt                string  `json:"created_at"`
}

// UpsertParams is the body of POST /v1/projects. Slug + Name are required;
// the remaining fields are optional and applied via COALESCE on update so
// nil values don't clobber existing rows.
type UpsertParams struct {
	Slug                     string
	Name                     string
	ForgeURL                 *string
	DefaultBranch            *string
	MattermostOutboxChannel  *string
	MattermostInboxChannel   *string
}

// Upsert inserts a new project or updates the existing one (matched by slug).
// Returns the full row. Idempotent.
func Upsert(ctx context.Context, pool *pgxpool.Pool, p UpsertParams) (*Project, error) {
	if p.Slug == "" {
		return nil, errors.New("slug required")
	}
	if p.Name == "" {
		return nil, errors.New("name required")
	}

	const q = `
		INSERT INTO projects (slug, name, forge_url, default_branch,
		                      mattermost_outbox_channel, mattermost_inbox_channel)
		VALUES ($1, $2, $3, COALESCE($4, 'main'), $5, $6)
		ON CONFLICT (slug) DO UPDATE
		SET name = EXCLUDED.name,
		    forge_url = COALESCE(EXCLUDED.forge_url, projects.forge_url),
		    default_branch = COALESCE(EXCLUDED.default_branch, projects.default_branch),
		    mattermost_outbox_channel = COALESCE(EXCLUDED.mattermost_outbox_channel, projects.mattermost_outbox_channel),
		    mattermost_inbox_channel = COALESCE(EXCLUDED.mattermost_inbox_channel, projects.mattermost_inbox_channel)
		RETURNING id, slug, name, forge_url, default_branch,
		          mattermost_outbox_channel, mattermost_inbox_channel,
		          created_at::text`

	var proj Project
	err := pool.QueryRow(ctx, q,
		p.Slug, p.Name, p.ForgeURL, p.DefaultBranch,
		p.MattermostOutboxChannel, p.MattermostInboxChannel,
	).Scan(&proj.ID, &proj.Slug, &proj.Name, &proj.ForgeURL, &proj.DefaultBranch,
		&proj.MattermostOutboxChannel, &proj.MattermostInboxChannel, &proj.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert project: %w", err)
	}
	return &proj, nil
}

// List returns every project ordered by slug. Used by /agent-events-health
// diagnostics on the plugin side.
func List(ctx context.Context, pool *pgxpool.Pool) ([]*Project, error) {
	const q = `
		SELECT id, slug, name, forge_url, default_branch,
		       mattermost_outbox_channel, mattermost_inbox_channel,
		       created_at::text
		  FROM projects
		 ORDER BY slug`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	out := make([]*Project, 0)
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.ForgeURL, &p.DefaultBranch,
			&p.MattermostOutboxChannel, &p.MattermostInboxChannel, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return out, nil
}

// GetBySlug returns one project. Wraps pgx.ErrNoRows; callers translate to 404.
func GetBySlug(ctx context.Context, pool *pgxpool.Pool, slug string) (*Project, error) {
	if slug == "" {
		return nil, errors.New("slug required")
	}
	const q = `
		SELECT id, slug, name, forge_url, default_branch,
		       mattermost_outbox_channel, mattermost_inbox_channel,
		       created_at::text
		  FROM projects
		 WHERE slug = $1`
	var p Project
	err := pool.QueryRow(ctx, q, slug).Scan(
		&p.ID, &p.Slug, &p.Name, &p.ForgeURL, &p.DefaultBranch,
		&p.MattermostOutboxChannel, &p.MattermostInboxChannel, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("get project: %w", err)
	}
	return &p, nil
}
