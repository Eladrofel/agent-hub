package server

// Operator-only read endpoints added in v0.1.7 (issue #43). All three live
// under the admin-token middleware: they expose fleet-wide state that's
// useful for the operator's diagnostics but not for individual per-host
// agents (which already have narrow-scope inspection via their own per-host
// bearer).
//
// Endpoints:
//
//	GET /v1/agents               — full fleet listing
//	GET /v1/events               — paginated event history with filters
//	GET /v1/health/full          — extended health with per-subsystem detail
//
// Pagination on /v1/events uses an opaque cursor over (created_at, id) so
// pages stay stable even as new events arrive — a raw OFFSET would skip
// rows the next page should have seen.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// =============================================================================
// GET /v1/agents
// =============================================================================

type agentListItem struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Alias              *string        `json:"alias,omitempty"` // mattermost_username; "Splinter" / "Mikey" / etc.
	Role               *string        `json:"role,omitempty"`
	HostKind           *string        `json:"host_kind,omitempty"`
	VMHostname         *string        `json:"vm_hostname,omitempty"`
	JoinedAt           time.Time      `json:"joined_at"`
	LastSeenAt         *time.Time     `json:"last_seen_at,omitempty"`
	Capabilities       []any          `json:"capabilities"`
	Metadata           map[string]any `json:"metadata"`
	ChannelMemberships []string       `json:"channel_memberships"`
}

type agentListResponse struct {
	Agents []agentListItem `json:"agents"`
	Count  int             `json:"count"`
}

func (a *App) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	const q = `
		SELECT id, name, mattermost_username, role, host_kind, vm_hostname,
		       created_at, last_seen_at, capabilities, metadata
		  FROM agents
		 ORDER BY created_at ASC`

	rows, err := a.Store.Pool.Query(r.Context(), q)
	if err != nil {
		a.Logger.Error("agents list query failed", "err", err)
		writeError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}
	defer rows.Close()

	out := []agentListItem{}
	for rows.Next() {
		var (
			item    agentListItem
			capsRaw []byte
			metaRaw []byte
		)
		if err := rows.Scan(&item.ID, &item.Name, &item.Alias,
			&item.Role, &item.HostKind, &item.VMHostname,
			&item.JoinedAt, &item.LastSeenAt, &capsRaw, &metaRaw); err != nil {
			a.Logger.Error("agents list scan failed", "err", err)
			writeError(w, http.StatusInternalServerError, "scan_failed", err.Error())
			return
		}
		if len(capsRaw) > 0 {
			_ = json.Unmarshal(capsRaw, &item.Capabilities)
		}
		if item.Capabilities == nil {
			item.Capabilities = []any{}
		}
		if len(metaRaw) > 0 {
			_ = json.Unmarshal(metaRaw, &item.Metadata)
		}
		if item.Metadata == nil {
			item.Metadata = map[string]any{}
		}
		// Channel memberships are not authoritatively tracked in the
		// events plane — Mattermost owns that state. Surface what we
		// know: the per-project mattermost_outbox_channel for projects
		// this agent has emitted events into. Empty list when none.
		item.ChannelMemberships = a.channelMembershipsFor(r.Context(), item.ID)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "rows_err", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, agentListResponse{Agents: out, Count: len(out)})
}

// channelMembershipsFor returns the distinct mattermost_outbox_channel
// values for projects this agent has emitted events into. Best-effort: a
// query failure logs and returns an empty list rather than failing the
// whole listing.
func (a *App) channelMembershipsFor(ctx context.Context, agentID string) []string {
	const q = `
		SELECT DISTINCT p.mattermost_outbox_channel
		  FROM events e
		  JOIN projects p ON p.id = e.project_id
		 WHERE e.agent_id = $1
		   AND p.mattermost_outbox_channel IS NOT NULL
		   AND p.mattermost_outbox_channel <> ''`
	rows, err := a.Store.Pool.Query(ctx, q, agentID)
	if err != nil {
		a.Logger.Warn("channel memberships query failed", "agent_id", agentID, "err", err)
		return []string{}
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// =============================================================================
// GET /v1/events?since=…&agent=…&type=…&limit=…&cursor=…
// =============================================================================

const (
	eventsDefaultLimit = 100
	eventsMaxLimit     = 500
)

type eventListItem struct {
	ID              string         `json:"id"`
	EventType       string         `json:"event_type"`
	EventVersion    int            `json:"event_version"`
	AgentID         *string        `json:"agent_id,omitempty"`
	AgentName       *string        `json:"agent_name,omitempty"`
	ProjectID       *string        `json:"project_id,omitempty"`
	TaskID          *string        `json:"task_id,omitempty"`
	AgentSessionID  *string        `json:"agent_session_id,omitempty"`
	ClaudeSessionID *string        `json:"claude_session_id,omitempty"`
	CorrelationID   *string        `json:"correlation_id,omitempty"`
	ActorType       string         `json:"actor_type"`
	ActorName       *string        `json:"actor_name,omitempty"`
	Branch          *string        `json:"branch,omitempty"`
	GitHeadSHA      *string        `json:"git_head_sha,omitempty"`
	Summary         *string        `json:"summary,omitempty"`
	Payload         map[string]any `json:"payload"`
	CreatedAt       time.Time      `json:"created_at"`
}

type eventListResponse struct {
	Events     []eventListItem `json:"events"`
	Count      int             `json:"count"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// eventCursor encodes the (created_at, id) tuple of the last row of the
// previous page. Opaque base64-url JSON — clients should NOT inspect it.
type eventCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"i"`
}

func encodeCursor(c eventCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (*eventCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode cursor base64: %w", err)
	}
	var c eventCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode cursor json: %w", err)
	}
	if c.ID == "" {
		return nil, errors.New("cursor missing id")
	}
	return &c, nil
}

func (a *App) handleEventsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := eventsDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeErrorWithDetails(w, http.StatusBadRequest, "invalid_limit",
				"limit must be a positive integer",
				map[string]string{"got": raw})
			return
		}
		if n > eventsMaxLimit {
			n = eventsMaxLimit
		}
		limit = n
	}

	var since *time.Time
	if raw := q.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErrorWithDetails(w, http.StatusBadRequest, "invalid_since",
				"since must be RFC3339",
				map[string]string{"got": raw})
			return
		}
		since = &t
	}

	agentFilter := strings.TrimSpace(q.Get("agent"))
	typeFilter := strings.TrimSpace(q.Get("type"))

	cur, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		writeErrorWithDetails(w, http.StatusBadRequest, "invalid_cursor",
			"cursor is malformed",
			map[string]string{"reason": err.Error()})
		return
	}

	// Resolve agent name → id if filter provided.
	var agentID *string
	if agentFilter != "" {
		var id string
		err := a.Store.Pool.QueryRow(r.Context(),
			`SELECT id FROM agents WHERE name = $1`, agentFilter).Scan(&id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeErrorWithDetails(w, http.StatusUnprocessableEntity, "unknown_agent",
					"no agent with that name",
					map[string]string{"agent": agentFilter})
				return
			}
			writeError(w, http.StatusInternalServerError, "agent_lookup_failed", err.Error())
			return
		}
		agentID = &id
	}

	// Build the SQL with positional args. Cursor is a strict-less-than
	// (created_at, id) so the next page picks up where the previous one
	// stopped (newest-first ordering).
	args := []any{}
	conds := []string{}
	add := func(c string, vals ...any) {
		args = append(args, vals...)
		conds = append(conds, c)
	}
	if since != nil {
		add(fmt.Sprintf("e.created_at >= $%d", len(args)+1), *since)
	}
	if agentID != nil {
		add(fmt.Sprintf("e.agent_id = $%d", len(args)+1), *agentID)
	}
	if typeFilter != "" {
		add(fmt.Sprintf("e.event_type = $%d", len(args)+1), typeFilter)
	}
	if cur != nil {
		// Newest-first: next page is rows STRICTLY OLDER than cursor row.
		add(fmt.Sprintf("(e.created_at, e.id) < ($%d, $%d)", len(args)+1, len(args)+2),
			cur.CreatedAt, cur.ID)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	// Fetch limit+1 to detect whether there's a next page.
	args = append(args, limit+1)
	sql := fmt.Sprintf(`
		SELECT e.id, e.event_type, e.event_version,
		       e.agent_id, a.name,
		       e.project_id, e.task_id, e.agent_session_id, e.claude_session_id,
		       e.correlation_id, e.actor_type, e.actor_name,
		       e.branch, e.git_head_sha,
		       e.summary, e.payload, e.created_at
		  FROM events e
		  LEFT JOIN agents a ON a.id = e.agent_id
		 %s
		 ORDER BY e.created_at DESC, e.id DESC
		 LIMIT $%d`, where, len(args))

	rows, err := a.Store.Pool.Query(r.Context(), sql, args...)
	if err != nil {
		a.Logger.Error("events list query failed", "err", err)
		writeError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}
	defer rows.Close()

	out := []eventListItem{}
	for rows.Next() {
		var (
			item       eventListItem
			payloadRaw []byte
		)
		if err := rows.Scan(&item.ID, &item.EventType, &item.EventVersion,
			&item.AgentID, &item.AgentName,
			&item.ProjectID, &item.TaskID, &item.AgentSessionID, &item.ClaudeSessionID,
			&item.CorrelationID, &item.ActorType, &item.ActorName,
			&item.Branch, &item.GitHeadSHA,
			&item.Summary, &payloadRaw, &item.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "scan_failed", err.Error())
			return
		}
		if len(payloadRaw) > 0 {
			_ = json.Unmarshal(payloadRaw, &item.Payload)
		}
		if item.Payload == nil {
			item.Payload = map[string]any{}
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "rows_err", err.Error())
		return
	}

	// Trim the +1 row and compute next-cursor if there's more.
	nextCursor := ""
	if len(out) > limit {
		last := out[limit-1]
		nextCursor = encodeCursor(eventCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		out = out[:limit]
	}

	writeJSON(w, http.StatusOK, eventListResponse{
		Events:     out,
		Count:      len(out),
		NextCursor: nextCursor,
	})
}

// =============================================================================
// GET /v1/health/full
// =============================================================================

type subsystemHealth struct {
	Status string         `json:"status"` // "ok" | "degraded" | "unknown" | "error"
	Detail map[string]any `json:"detail,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type fullHealthResponse struct {
	Status            string                     `json:"status"`
	UptimeSeconds     float64                    `json:"uptime_seconds"`
	Version           string                     `json:"version"`
	GeneratedAt       time.Time                  `json:"generated_at"`
	Subsystems        map[string]subsystemHealth `json:"subsystems"`
	SanitiserPatterns int                        `json:"sanitiser_patterns"`
}

// gatewayStartTime is set in server.Run via App.StartedAt; we surface it
// here in seconds for the operator. Stored on App rather than as a package
// global so tests can pin a deterministic value.
func (a *App) handleHealthFull(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	subs := map[string]subsystemHealth{
		"db":             a.healthDB(ctx),
		"outbox_worker":  a.healthOutboxWorker(ctx),
		"inbox_webhook":  a.healthInboxWebhook(ctx),
		"mattermost":     a.healthMattermost(ctx),
		"failed_emits":   a.healthFailedEmits(ctx),
	}

	// Rollup status: if anything is "error" → error; else if any "degraded" → degraded; else ok.
	rollup := "ok"
	for _, s := range subs {
		switch s.Status {
		case "error":
			rollup = "error"
		case "degraded":
			if rollup != "error" {
				rollup = "degraded"
			}
		}
	}

	uptime := 0.0
	if !a.StartedAt.IsZero() {
		uptime = time.Since(a.StartedAt).Seconds()
	}

	writeJSON(w, http.StatusOK, fullHealthResponse{
		Status:            rollup,
		UptimeSeconds:     uptime,
		Version:           a.Version,
		GeneratedAt:       time.Now().UTC(),
		Subsystems:        subs,
		SanitiserPatterns: a.Sanitiser.Count(),
	})
}

func (a *App) healthDB(ctx context.Context) subsystemHealth {
	pingCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := a.Store.Ping(pingCtx); err != nil {
		return subsystemHealth{Status: "error", Error: err.Error()}
	}
	lag := time.Since(start)
	stat := a.Store.Pool.Stat()
	return subsystemHealth{
		Status: "ok",
		Detail: map[string]any{
			"ping_ms":             lag.Milliseconds(),
			"acquired_conns":      stat.AcquiredConns(),
			"idle_conns":          stat.IdleConns(),
			"max_conns":           stat.MaxConns(),
		},
	}
}

func (a *App) healthOutboxWorker(ctx context.Context) subsystemHealth {
	// "Last tick" is inferred from the most recent transition (sent_at or
	// updated row); the worker itself doesn't heartbeat to a table. Queue
	// depth is the count of pending rows. Stale-pending (created > 60s ago,
	// still pending) → degraded; >50 pending → degraded.
	var (
		pending     int
		stalePending int
		lastSentAt  *time.Time
	)
	if err := a.Store.Pool.QueryRow(ctx,
		`SELECT
		   (SELECT count(*) FROM mattermost_outbox WHERE status = 'pending'),
		   (SELECT count(*) FROM mattermost_outbox
		     WHERE status = 'pending'
		       AND created_at < now() - interval '60 seconds'),
		   (SELECT max(sent_at) FROM mattermost_outbox WHERE status = 'sent')`,
	).Scan(&pending, &stalePending, &lastSentAt); err != nil {
		return subsystemHealth{Status: "error", Error: err.Error()}
	}
	status := "ok"
	if stalePending > 0 || pending > 50 {
		status = "degraded"
	}
	detail := map[string]any{
		"queue_depth":      pending,
		"stale_pending":    stalePending,
	}
	if lastSentAt != nil {
		detail["last_sent_at"] = lastSentAt.UTC()
	}
	return subsystemHealth{Status: status, Detail: detail}
}

func (a *App) healthInboxWebhook(ctx context.Context) subsystemHealth {
	// Last received-at per agent: max(created_at) on mattermost_inbox grouped
	// by target_agent_id, joined to agents for human-readable names.
	rows, err := a.Store.Pool.Query(ctx, `
		SELECT a.name, max(mi.created_at)
		  FROM mattermost_inbox mi
		  JOIN agents a ON a.id = mi.target_agent_id
		 GROUP BY a.name
		 ORDER BY a.name`)
	if err != nil {
		return subsystemHealth{Status: "error", Error: err.Error()}
	}
	defer rows.Close()
	perAgent := map[string]time.Time{}
	for rows.Next() {
		var name string
		var ts time.Time
		if err := rows.Scan(&name, &ts); err != nil {
			continue
		}
		perAgent[name] = ts.UTC()
	}
	return subsystemHealth{
		Status: "ok", // receiver liveness lives in its own process; we report what we see in DB
		Detail: map[string]any{
			"last_received_per_agent": perAgent,
		},
	}
}

func (a *App) healthMattermost(ctx context.Context) subsystemHealth {
	// The gateway itself doesn't hold an MM HTTP client (the outbox-worker
	// does, in a separate process). We infer reachability from recent
	// outbox-worker success: if any row went sent in the last 5 minutes,
	// MM is reachable. Otherwise "unknown" — not necessarily broken, just
	// no traffic to confirm.
	var recentSends int
	if err := a.Store.Pool.QueryRow(ctx,
		`SELECT count(*) FROM mattermost_outbox
		  WHERE status = 'sent'
		    AND sent_at > now() - interval '5 minutes'`,
	).Scan(&recentSends); err != nil {
		return subsystemHealth{Status: "error", Error: err.Error()}
	}
	status := "unknown"
	if recentSends > 0 {
		status = "ok"
	}
	return subsystemHealth{
		Status: status,
		Detail: map[string]any{
			"recent_sends_5m": recentSends,
		},
	}
}

func (a *App) healthFailedEmits(ctx context.Context) subsystemHealth {
	// "Failed-emit cache entries per peer" — the outbox stores failed rows
	// with status='failed'. Surface counts per agent (via the join through
	// events.agent_id).
	rows, err := a.Store.Pool.Query(ctx, `
		SELECT coalesce(a.name, 'unknown'), count(*)
		  FROM mattermost_outbox o
		  LEFT JOIN events e ON e.id = o.event_id
		  LEFT JOIN agents a ON a.id = e.agent_id
		 WHERE o.status = 'failed'
		 GROUP BY a.name
		 ORDER BY 2 DESC`)
	if err != nil {
		return subsystemHealth{Status: "error", Error: err.Error()}
	}
	defer rows.Close()
	perAgent := map[string]int{}
	total := 0
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			continue
		}
		perAgent[name] = n
		total += n
	}
	status := "ok"
	if total > 0 {
		status = "degraded"
	}
	return subsystemHealth{
		Status: status,
		Detail: map[string]any{
			"total":     total,
			"per_agent": perAgent,
		},
	}
}
