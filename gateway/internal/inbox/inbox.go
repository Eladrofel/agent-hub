// Package inbox owns mattermost_inbox. Pre-Component C there is no
// inbox-webhook writer, so polls return empty — but the endpoint is wired
// because Component B hooks (SessionStart, Stop) already call it.
package inbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Message struct {
	ID             string         `json:"id"`
	ProjectID      *string        `json:"project_id,omitempty"`
	TargetAgentID  string         `json:"target_agent_id"`
	SourceUsername *string        `json:"source_username,omitempty"`
	SourceChannel  *string        `json:"source_channel_id,omitempty"`
	SourcePost     *string        `json:"source_post_id,omitempty"`
	Body           string         `json:"message"`
	Props          map[string]any `json:"props,omitempty"`
	CreatedAt      string         `json:"created_at"`
}

// Poll returns undelivered messages for the agent, optionally filtered to
// those created after `since` (zero time means no lower bound). Marks
// returned rows as delivered in the same transaction so a subsequent poll
// from the same agent doesn't re-surface them.
func Poll(ctx context.Context, pool *pgxpool.Pool, agentID string, since time.Time) ([]Message, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	const selectQ = `
		SELECT id, project_id, target_agent_id, source_username,
		       source_channel_id, source_post_id, message, props, created_at
		  FROM mattermost_inbox
		 WHERE target_agent_id = $1
		   AND delivered_at IS NULL
		   AND ($2::timestamptz IS NULL OR created_at > $2)
		 ORDER BY created_at ASC`

	var sinceArg any
	if !since.IsZero() {
		sinceArg = since
	}

	rows, err := tx.Query(ctx, selectQ, agentID, sinceArg)
	if err != nil {
		return nil, err
	}

	var (
		out []Message
		ids []string
	)
	for rows.Next() {
		var (
			m         Message
			propsRaw  []byte
			createdAt time.Time
		)
		if err := rows.Scan(
			&m.ID, &m.ProjectID, &m.TargetAgentID, &m.SourceUsername,
			&m.SourceChannel, &m.SourcePost, &m.Body, &propsRaw, &createdAt,
		); err != nil {
			rows.Close()
			return nil, err
		}
		m.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		_ = json.Unmarshal(propsRaw, &m.Props)
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) > 0 {
		_, err = tx.Exec(ctx,
			`UPDATE mattermost_inbox
			    SET delivered_at = now()
			  WHERE id = ANY($1)`,
			ids)
		if err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
