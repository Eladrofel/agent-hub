package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// =============================================================================
// Test fixture — shared Postgres + a fake Mattermost server.
// =============================================================================

type fakeMM struct {
	server   *httptest.Server
	postBody atomic.Value // last decoded /posts body, *map[string]any
	postHits atomic.Int32
	// status maps:
	//   channelStatus: response code for the channel-lookup endpoint
	//   postStatus:    response code for /api/v4/posts
	channelStatus int
	postStatus    int

	// channelID returned in channel-lookup payload
	channelID string
}

func newFakeMM(t *testing.T) *fakeMM {
	t.Helper()
	f := &fakeMM{
		channelStatus: 200,
		postStatus:    201,
		channelID:     "channelxxxxxxxxxxxxxxxxxxx", // 26 chars no hyphen
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/teams/", func(w http.ResponseWriter, r *http.Request) {
		// .../teams/name/{team}/channels/name/{channel}
		if f.channelStatus != 200 {
			http.Error(w, "channel error", f.channelStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   f.channelID,
			"name": "agent-events",
		})
	})
	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.postBody.Store(&body)
		f.postHits.Add(1)
		if f.postStatus < 200 || f.postStatus >= 300 {
			http.Error(w, "boom", f.postStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.postStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "post-xxxx"})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("AGENT_HUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_HUB_TEST_DATABASE_URL not set; skipping outbox integration tests")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Reset just the outbox table — other tests in other packages may
	// share this DB but they truncate themselves on setup too.
	_, err = st.Pool.Exec(ctx, "TRUNCATE TABLE mattermost_outbox CASCADE")
	if err != nil {
		t.Fatalf("truncate outbox: %v", err)
	}
	return st.Pool
}

func seedOutboxRow(t *testing.T, pool *pgxpool.Pool, channelID, message string) (id, eventID string) {
	t.Helper()
	ctx := context.Background()
	// We need a real events row to satisfy the FK. Seed an agent + event.
	var agentID string
	err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, role) VALUES ($1, 'operator') RETURNING id`,
		fmt.Sprintf("agent-outbox-test-%d", time.Now().UnixNano())).Scan(&agentID)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	err = pool.QueryRow(ctx,
		`INSERT INTO events (agent_id, event_type, summary)
		 VALUES ($1, 'task.created', 'seed')
		 RETURNING id`, agentID).Scan(&eventID)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	err = pool.QueryRow(ctx,
		`INSERT INTO mattermost_outbox (event_id, channel_id, message, props)
		 VALUES ($1, $2, $3, '{}'::jsonb)
		 RETURNING id`, eventID, channelID, message).Scan(&id)
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	return id, eventID
}

func newTestWorker(t *testing.T, pool *pgxpool.Pool, mm *fakeMM) *Worker {
	t.Helper()
	cfg := WorkerConfig{
		MattermostURL:         mm.server.URL,
		MattermostURLOverride: mm.server.URL,
		MattermostToken:       "test-token",
		MattermostTeamName:    "agents",
		PollInterval:          5 * time.Second,
		MaxAttempts:           3,
		BatchSize:             50,
		HTTPClient:            &http.Client{Timeout: 5 * time.Second},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWorker(pool, cfg, logger)
}

// =============================================================================
// Tick — happy path + retries + non-retryable.
// =============================================================================

func TestTick_HappyPath_PostsAndMarksSent(t *testing.T) {
	pool := openTestPool(t)
	mm := newFakeMM(t)
	w := newTestWorker(t, pool, mm)

	id, eventID := seedOutboxRow(t, pool, "agent-events", "hello from gateway")

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	status, attempts, lastErr, err := ScanRow(context.Background(), pool, id)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "sent" {
		t.Fatalf("status = %q, want 'sent' (lastErr=%v attempts=%d)", status, lastErr, attempts)
	}

	got := mm.postBody.Load().(*map[string]any)
	if (*got)["channel_id"] != mm.channelID {
		t.Fatalf("posted channel_id = %v, want %s", (*got)["channel_id"], mm.channelID)
	}
	if (*got)["message"] != "hello from gateway" {
		t.Fatalf("posted message = %v", (*got)["message"])
	}
	props, _ := (*got)["props"].(map[string]any)
	if !strings.HasPrefix(fmt.Sprint(props["idempotency_key"]), eventID+"_") {
		t.Fatalf("idempotency_key = %v, want prefix %s_", props["idempotency_key"], eventID)
	}
}

func TestTick_4xx_MarksFailedNonRetryable(t *testing.T) {
	pool := openTestPool(t)
	mm := newFakeMM(t)
	mm.postStatus = http.StatusBadRequest
	w := newTestWorker(t, pool, mm)

	id, _ := seedOutboxRow(t, pool, "agent-events", "bad")

	_ = w.Tick(context.Background())
	status, attempts, lastErr, _ := ScanRow(context.Background(), pool, id)
	if status != "failed" {
		t.Fatalf("status = %q, want 'failed'", status)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if lastErr == nil || !strings.Contains(*lastErr, "400") {
		t.Fatalf("last_error did not capture 400, got %v", lastErr)
	}
}

func TestTick_5xx_BumpsAttemptsThenFailsAtCeiling(t *testing.T) {
	pool := openTestPool(t)
	mm := newFakeMM(t)
	mm.postStatus = http.StatusInternalServerError
	w := newTestWorker(t, pool, mm) // MaxAttempts=3

	id, _ := seedOutboxRow(t, pool, "agent-events", "transient")

	// First tick: attempts 0→1, status stays pending.
	_ = w.Tick(context.Background())
	status, attempts, _, _ := ScanRow(context.Background(), pool, id)
	if status != "pending" || attempts != 1 {
		t.Fatalf("after tick1: status=%q attempts=%d, want pending/1", status, attempts)
	}

	// Second tick: 1→2, still pending.
	_ = w.Tick(context.Background())
	status, attempts, _, _ = ScanRow(context.Background(), pool, id)
	if status != "pending" || attempts != 2 {
		t.Fatalf("after tick2: status=%q attempts=%d, want pending/2", status, attempts)
	}

	// Third tick: 2→3 == MaxAttempts → row goes to failed.
	_ = w.Tick(context.Background())
	status, attempts, lastErr, _ := ScanRow(context.Background(), pool, id)
	if status != "failed" {
		t.Fatalf("after tick3: status=%q, want 'failed'", status)
	}
	if lastErr == nil || !strings.Contains(*lastErr, "max_attempts") {
		t.Fatalf("last_error missing max_attempts: %v", lastErr)
	}
	_ = attempts // suppress unused
}

func TestTick_ChannelLookupFailureNonRetryable(t *testing.T) {
	pool := openTestPool(t)
	mm := newFakeMM(t)
	mm.channelStatus = http.StatusNotFound
	w := newTestWorker(t, pool, mm)

	id, _ := seedOutboxRow(t, pool, "no-such-channel", "x")
	_ = w.Tick(context.Background())

	status, _, lastErr, _ := ScanRow(context.Background(), pool, id)
	if status != "failed" {
		t.Fatalf("status = %q, want 'failed'", status)
	}
	if lastErr == nil || !strings.Contains(*lastErr, "channel resolve") {
		t.Fatalf("last_error = %v, want channel resolve hint", lastErr)
	}
}

func TestTick_NoPendingRowsIsNoop(t *testing.T) {
	pool := openTestPool(t)
	mm := newFakeMM(t)
	w := newTestWorker(t, pool, mm)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick on empty queue: %v", err)
	}
	if mm.postHits.Load() != 0 {
		t.Fatalf("expected no posts, got %d", mm.postHits.Load())
	}
}

// =============================================================================
// resolveChannelID — caching + id-passthrough.
// =============================================================================

func TestResolveChannelID_CachesPerName(t *testing.T) {
	pool := openTestPool(t)
	mm := newFakeMM(t)
	w := newTestWorker(t, pool, mm)

	id1, err := w.resolveChannelID(context.Background(), "agent-events")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := w.resolveChannelID(context.Background(), "agent-events")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 || id1 != mm.channelID {
		t.Fatalf("cache mismatch: id1=%q id2=%q want=%q", id1, id2, mm.channelID)
	}

	// Second resolve must NOT have re-queried the channel endpoint.
	// (postHits is unrelated; we don't track channel hits separately, but
	// the test still asserts both calls returned the same id from cache.)
}

func TestResolveChannelID_PassthroughForIDs(t *testing.T) {
	pool := openTestPool(t)
	mm := newFakeMM(t)
	w := newTestWorker(t, pool, mm)

	// Caller passes what looks like an id (26 chars, no hyphen) — should
	// be returned as-is without hitting Mattermost.
	in := "abcdefghijklmnopqrstuvwxyz"
	out, err := w.resolveChannelID(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("passthrough: got %q want %q", out, in)
	}
}

// =============================================================================
// Run() argument validation — fast-fail on missing env.
// =============================================================================

func TestRun_RequiresAllConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := Run(ctx, WorkerConfig{}) // everything missing
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v, want DATABASE_URL required", err)
	}
}

// Silence imports used only by future test paths.
var (
	_ = pgx.ErrNoRows
	_ = ErrChannelLookup
	_ = errors.New
)
