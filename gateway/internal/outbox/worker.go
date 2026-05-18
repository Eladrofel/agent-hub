// Worker loop for the mattermost_outbox table.
//
// Polls pending rows on a ticker; for each row resolves the channel name to
// a Mattermost channel id (cached in-process), POSTs to /api/v4/posts, then
// transitions the row by HTTP-class:
//
//	2xx           → status='sent', sent_at=now()
//	4xx           → status='failed', last_error=<reason>  (non-retryable)
//	5xx / network → attempts++; status stays 'pending'    (retry later)
//
// After attempts >= maxAttempts a still-pending row is downgraded to
// 'failed' so the queue can't grow unbounded behind a poisoned message.
//
// Idempotency: each POST carries props.idempotency_key = "<event_id>_<attempt>".
// Mattermost itself doesn't use this for dedupe today (v9.x doesn't natively
// dedupe), but the field is forwarded to integrations/audit and lets us trace
// duplicate posts if Mattermost ever does.
package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// WorkerConfig is the runtime configuration for `agent-hub outbox-worker`.
// Fields map 1:1 to env vars consumed in cmd/agent-hub/main.go.
type WorkerConfig struct {
	DatabaseURL          string        // DATABASE_URL
	MattermostURL        string        // MATTERMOST_URL (e.g., https://mm.example)
	MattermostToken      string        // MATTERMOST_TOKEN (service-account PAT)
	MattermostTeamName   string        // MATTERMOST_TEAM_NAME (for channel-name resolution)
	PollInterval         time.Duration // POLL_INTERVAL_SECONDS
	MaxAttempts          int           // hard ceiling before a row is failed
	BatchSize            int           // rows to SELECT per poll
	HTTPClient           *http.Client  // injected in tests; nil = default
	MattermostURLOverride string       // tests inject a mock-server base here
}

// DefaultWorkerConfig pulls a WorkerConfig from process env with the
// production defaults filled in. Caller wires this into cmd/agent-hub.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		MattermostURL:      os.Getenv("MATTERMOST_URL"),
		MattermostToken:    os.Getenv("MATTERMOST_TOKEN"),
		MattermostTeamName: os.Getenv("MATTERMOST_TEAM_NAME"),
		PollInterval:       envDuration("POLL_INTERVAL_SECONDS", 5*time.Second),
		MaxAttempts:        5,
		BatchSize:          20,
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}

// Run boots the outbox-worker and blocks until ctx is cancelled. Returns
// nil on graceful shutdown; non-nil only on unrecoverable startup errors
// (DB unreachable, missing config). Per-post failures are logged and
// recorded in the row's last_error; they never crash the worker.
func Run(ctx context.Context, cfg WorkerConfig) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	if cfg.MattermostURL == "" {
		return errors.New("MATTERMOST_URL is required")
	}
	if cfg.MattermostToken == "" {
		return errors.New("MATTERMOST_TOKEN is required")
	}
	if cfg.MattermostTeamName == "" {
		return errors.New("MATTERMOST_TEAM_NAME is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	w := NewWorker(st.Pool, cfg, logger)
	logger.Info("outbox-worker starting",
		"mattermost_url", cfg.MattermostURL,
		"team", cfg.MattermostTeamName,
		"poll_interval", cfg.PollInterval.String(),
		"batch_size", cfg.BatchSize)

	return w.Loop(ctx)
}

// Worker is the testable unit. NewWorker constructs it; Loop runs it; Tick
// is the per-poll body that tests can invoke directly without a ticker.
type Worker struct {
	pool   *pgxpool.Pool
	cfg    WorkerConfig
	logger *slog.Logger
	http   *http.Client

	mu           sync.Mutex
	channelCache map[string]string // channel-name → channel-id
}

func NewWorker(pool *pgxpool.Pool, cfg WorkerConfig, logger *slog.Logger) *Worker {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Worker{
		pool:         pool,
		cfg:          cfg,
		logger:       logger,
		http:         hc,
		channelCache: make(map[string]string),
	}
}

// Loop runs Tick on cfg.PollInterval until ctx is cancelled. Per-tick
// errors are logged but never bubble up — the loop survives transient DB
// or Mattermost failures.
func (w *Worker) Loop(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	// Tick once immediately so a freshly-started worker doesn't sit on
	// pending rows for one interval.
	if err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.logger.Warn("outbox tick failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("outbox-worker shutting down")
			return nil
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Warn("outbox tick failed", "err", err)
			}
		}
	}
}

// Tick processes one batch of pending rows. Returns the first
// non-context-cancellation error encountered, if any (the caller logs it
// and continues).
func (w *Worker) Tick(ctx context.Context) error {
	rows, err := w.pool.Query(ctx,
		`SELECT id, channel_id, message, props, attempts, event_id
		   FROM mattermost_outbox
		  WHERE status = 'pending'
		  ORDER BY created_at ASC
		  LIMIT $1`,
		w.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("select pending outbox rows: %w", err)
	}

	type pendingRow struct {
		id        string
		channelID string
		message   string
		propsRaw  []byte
		attempts  int
		eventID   string
	}
	var pending []pendingRow
	for rows.Next() {
		var p pendingRow
		if err := rows.Scan(&p.id, &p.channelID, &p.message, &p.propsRaw, &p.attempts, &p.eventID); err != nil {
			rows.Close()
			return fmt.Errorf("scan outbox row: %w", err)
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate outbox rows: %w", err)
	}

	for _, p := range pending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Compose props with a fresh idempotency_key based on the
		// CURRENT attempt number so retries are distinguishable.
		props := map[string]any{}
		if len(p.propsRaw) > 0 {
			_ = json.Unmarshal(p.propsRaw, &props)
		}
		props["idempotency_key"] = fmt.Sprintf("%s_%d", p.eventID, p.attempts)

		channelID, resolveErr := w.resolveChannelID(ctx, p.channelID)
		if resolveErr != nil {
			w.logger.Warn("channel resolve failed; treating as 4xx",
				"outbox_id", p.id, "channel", p.channelID, "err", resolveErr)
			w.markFailed(ctx, p.id, fmt.Sprintf("channel resolve: %v", resolveErr))
			continue
		}

		status, postErr := w.postOne(ctx, channelID, p.message, props)
		switch {
		case postErr == nil && status >= 200 && status < 300:
			w.markSent(ctx, p.id)
		case status >= 400 && status < 500:
			w.markFailed(ctx, p.id, fmt.Sprintf("mattermost %d: %v", status, postErr))
		default:
			// 5xx, network error, timeout → retry.
			reason := "retry"
			if postErr != nil {
				reason = postErr.Error()
			}
			w.bumpAttempts(ctx, p.id, p.attempts+1, reason)
		}
	}
	return nil
}

// resolveChannelID returns the Mattermost internal channel id for a
// channel-name string. If the caller already passed an id (heuristic: long
// alphanumeric, length 26 which is Mattermost's id format), we accept it
// as-is. Cache is in-process; a long-running worker only pays the lookup
// once per channel.
func (w *Worker) resolveChannelID(ctx context.Context, channelOrName string) (string, error) {
	// Mattermost channel IDs are 26 chars of [a-z0-9]; channel names allow
	// hyphens. A name like "agent-events" has a hyphen; an id never does.
	// Cheap heuristic, good enough for v0.1.3.
	if len(channelOrName) == 26 && !containsHyphen(channelOrName) {
		return channelOrName, nil
	}

	w.mu.Lock()
	if id, ok := w.channelCache[channelOrName]; ok {
		w.mu.Unlock()
		return id, nil
	}
	w.mu.Unlock()

	id, err := w.lookupChannelID(ctx, channelOrName)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	w.channelCache[channelOrName] = id
	w.mu.Unlock()
	return id, nil
}

func containsHyphen(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			return true
		}
	}
	return false
}

func (w *Worker) lookupChannelID(ctx context.Context, channelName string) (string, error) {
	base := w.mattermostBase()
	u := fmt.Sprintf("%s/api/v4/teams/name/%s/channels/name/%s",
		base, url.PathEscape(w.cfg.MattermostTeamName), url.PathEscape(channelName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.MattermostToken)
	resp, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("lookup channel %q: HTTP %d: %s", channelName, resp.StatusCode, body)
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		return "", fmt.Errorf("decode channel lookup: %w", err)
	}
	if got.ID == "" {
		return "", fmt.Errorf("channel %q has empty id in lookup response", channelName)
	}
	return got.ID, nil
}

// postOne sends one /api/v4/posts request. Returns the HTTP status (0 on
// pre-flight errors like context cancellation) and a non-nil error iff
// either the transport failed or the status is non-2xx.
func (w *Worker) postOne(ctx context.Context, channelID, message string, props map[string]any) (int, error) {
	body, err := json.Marshal(map[string]any{
		"channel_id": channelID,
		"message":    message,
		"props":      props,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal post body: %w", err)
	}
	u := w.mattermostBase() + "/api/v4/posts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.MattermostToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("mattermost POST /posts: %d: %s", resp.StatusCode, respBody)
	}
	return resp.StatusCode, nil
}

func (w *Worker) mattermostBase() string {
	if w.cfg.MattermostURLOverride != "" {
		return w.cfg.MattermostURLOverride
	}
	return w.cfg.MattermostURL
}

func (w *Worker) markSent(ctx context.Context, id string) {
	_, err := w.pool.Exec(ctx,
		`UPDATE mattermost_outbox
		    SET status = 'sent', sent_at = now(), last_error = NULL
		  WHERE id = $1`, id)
	if err != nil {
		w.logger.Error("mark sent failed", "id", id, "err", err)
	}
}

func (w *Worker) markFailed(ctx context.Context, id, reason string) {
	_, err := w.pool.Exec(ctx,
		`UPDATE mattermost_outbox
		    SET status = 'failed', last_error = $2, attempts = attempts + 1
		  WHERE id = $1`, id, reason)
	if err != nil {
		w.logger.Error("mark failed failed", "id", id, "err", err)
	}
}

func (w *Worker) bumpAttempts(ctx context.Context, id string, newAttempts int, reason string) {
	// If we've hit the ceiling, fail the row rather than leaving it pending
	// forever.
	if newAttempts >= w.cfg.MaxAttempts {
		w.markFailed(ctx, id, fmt.Sprintf("max_attempts(%d) exhausted: %s", w.cfg.MaxAttempts, reason))
		return
	}
	_, err := w.pool.Exec(ctx,
		`UPDATE mattermost_outbox
		    SET attempts = $2, last_error = $3
		  WHERE id = $1`, id, newAttempts, reason)
	if err != nil {
		w.logger.Error("bump attempts failed", "id", id, "err", err)
	}
}

// Sentinel used in tests to verify error wrapping.
var ErrChannelLookup = errors.New("channel lookup failed")

// scanRow is a small helper used in tests to fetch outbox row state by id.
// Exposed here (rather than in a _test.go file) so external test packages
// can also use it if needed.
func ScanRow(ctx context.Context, pool *pgxpool.Pool, id string) (status string, attempts int, lastErr *string, err error) {
	err = pool.QueryRow(ctx,
		`SELECT status, attempts, last_error FROM mattermost_outbox WHERE id = $1`,
		id).Scan(&status, &attempts, &lastErr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, nil, fmt.Errorf("outbox row %s: %w", id, pgx.ErrNoRows)
		}
		return "", 0, nil, err
	}
	return
}
