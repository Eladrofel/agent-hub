// Package agents owns the agents table. The auth middleware reads from it on
// every request (token-hash lookup); register lets a peer fill in role +
// host metadata after mint-token has stamped the bcrypt'd token.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Agent is the public-facing shape (no token_hash).
type Agent struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Role               *string        `json:"role,omitempty"`
	HostKind           *string        `json:"host_kind,omitempty"`
	VMHostname         *string        `json:"vm_hostname,omitempty"`
	MattermostUsername *string        `json:"mattermost_username,omitempty"`
	Permissions        map[string]any `json:"permissions,omitempty"`
	Capabilities       []any          `json:"capabilities,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

// RegisterParams is the body of POST /v1/agents/register. Name must match
// the authenticated agent's name — enforced at the handler boundary.
type RegisterParams struct {
	Name               string
	Role               *string
	HostKind           *string
	VMHostname         *string
	MattermostUsername *string
	Capabilities       []any
	Metadata           map[string]any
}

// Register updates the matched agent's mutable fields. Idempotent.
func Register(ctx context.Context, pool *pgxpool.Pool, p RegisterParams) (*Agent, error) {
	if p.Name == "" {
		return nil, errors.New("name required")
	}

	capJSON, err := json.Marshal(coalesceSlice(p.Capabilities))
	if err != nil {
		return nil, fmt.Errorf("marshal capabilities: %w", err)
	}
	metaJSON, err := json.Marshal(coalesceMap(p.Metadata))
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}

	const q = `
		UPDATE agents
		   SET role = COALESCE($2, role),
		       host_kind = COALESCE($3, host_kind),
		       vm_hostname = COALESCE($4, vm_hostname),
		       mattermost_username = COALESCE($5, mattermost_username),
		       capabilities = $6,
		       metadata = $7,
		       last_seen_at = now()
		 WHERE name = $1
		 RETURNING id, name, role, host_kind, vm_hostname,
		           mattermost_username, permissions, capabilities, metadata`

	var (
		a              Agent
		permsRaw       []byte
		capsRaw        []byte
		metaRaw        []byte
	)
	err = pool.QueryRow(ctx, q,
		p.Name, p.Role, p.HostKind, p.VMHostname, p.MattermostUsername,
		capJSON, metaJSON,
	).Scan(&a.ID, &a.Name, &a.Role, &a.HostKind, &a.VMHostname,
		&a.MattermostUsername, &permsRaw, &capsRaw, &metaRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("agent name %q: %w", p.Name, pgx.ErrNoRows)
		}
		return nil, fmt.Errorf("update agent: %w", err)
	}
	_ = json.Unmarshal(permsRaw, &a.Permissions)
	_ = json.Unmarshal(capsRaw, &a.Capabilities)
	_ = json.Unmarshal(metaRaw, &a.Metadata)
	return &a, nil
}

// MintToken upserts the agent row and stamps a new bcrypt hash. Called by
// the admin endpoint. Returns the agent's id (callers already hold the
// plaintext token they minted).
func MintToken(ctx context.Context, pool *pgxpool.Pool, name, tokenHash string) (string, error) {
	if name == "" {
		return "", errors.New("name required")
	}
	if tokenHash == "" {
		return "", errors.New("token_hash required")
	}
	const q = `
		INSERT INTO agents (name, token_hash)
		VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE
		SET token_hash = EXCLUDED.token_hash
		RETURNING id`
	var id string
	if err := pool.QueryRow(ctx, q, name, tokenHash).Scan(&id); err != nil {
		return "", fmt.Errorf("upsert agent: %w", err)
	}
	return id, nil
}

// ResolveIDByName is used by inbox poll + future operator endpoints that
// accept an agent_name query param.
func ResolveIDByName(ctx context.Context, pool *pgxpool.Pool, name string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `SELECT id FROM agents WHERE name = $1`, name).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

func coalesceMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func coalesceSlice(s []any) []any {
	if s == nil {
		return []any{}
	}
	return s
}
