// Package outbox owns the mattermost_outbox table — the gateway side of the
// at-least-once Mattermost delivery pipeline. POST /v1/events writes an
// outbox row in the SAME transaction as the events insert when event_type is
// in CuratedTypes, so curated events are durable-and-queued atomically with
// their underlying ledger row.
//
// The outbox-worker subcommand (see worker.go) drains pending rows by POSTing
// to Mattermost's /api/v4/posts; rows transition pending→sent on 2xx,
// pending→failed on 4xx (non-retryable) or after attempts >= maxAttempts.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// InsertParams is the per-event outbox row written by the events handler.
// ChannelID at this stage may be either a Mattermost internal channel id
// or a channel name string — the outbox-worker resolves names → ids
// lazily on its first post for that name (cached in-process).
type InsertParams struct {
	ProjectID *string
	TaskID    *string
	EventID   string
	ChannelID string
	Message   string
	Props     map[string]any
}

// InsertPending writes a single mattermost_outbox row inside the caller's
// transaction. Returns the new outbox row id, or an error wrapping the SQL
// failure. Status defaults to 'pending'; attempts to 0.
func InsertPending(ctx context.Context, tx pgx.Tx, p InsertParams) (string, error) {
	if p.EventID == "" {
		return "", errors.New("event_id required for outbox row")
	}
	if p.ChannelID == "" {
		return "", errors.New("channel_id required for outbox row")
	}
	propsJSON, err := json.Marshal(coalesceMap(p.Props))
	if err != nil {
		return "", fmt.Errorf("marshal outbox props: %w", err)
	}

	const q = `
		INSERT INTO mattermost_outbox (
			project_id, task_id, event_id,
			channel_id, message, props,
			status, attempts
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			'pending', 0
		)
		RETURNING id`

	var id string
	err = tx.QueryRow(ctx, q,
		p.ProjectID, p.TaskID, p.EventID,
		p.ChannelID, p.Message, propsJSON,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert mattermost_outbox: %w", err)
	}
	return id, nil
}

func coalesceMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
