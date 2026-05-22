package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// =============================================================================
// Fixture
// =============================================================================

const testWebhookSecret = "test-webhook-secret"

func openWebhookTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("AGENT_HUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_HUB_TEST_DATABASE_URL not set; skipping inbox-webhook integration tests")
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
	_, err = st.Pool.Exec(ctx, "TRUNCATE TABLE mattermost_inbox, agents CASCADE")
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st.Pool
}

func seedAgent(t *testing.T, pool *pgxpool.Pool, name, mmUsername string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	var mm any
	if mmUsername != "" {
		mm = mmUsername
	}
	err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, role, mattermost_username)
		 VALUES ($1, 'operator', $2)
		 RETURNING id`, name, mm).Scan(&id)
	if err != nil {
		t.Fatalf("seed agent %s: %v", name, err)
	}
	return id
}

func newTestHandler(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWebhookHandler(pool, testWebhookSecret, logger).Routes()
}

func formPost(t *testing.T, handler http.Handler, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	values := url.Values{}
	for k, v := range fields {
		values.Set(k, v)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inbox/webhook",
		strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func jsonPost(t *testing.T, handler http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/inbox/webhook", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// =============================================================================
// Token + method gating
// =============================================================================

func TestWebhook_BadTokenIs401NoBody(t *testing.T) {
	pool := openWebhookTestPool(t)
	h := newTestHandler(t, pool)
	w := formPost(t, h, map[string]string{
		"token": "wrong-secret",
		"text":  "@agent-1 hi",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Fatalf("expected empty body, got %q", w.Body.String())
	}
}

func TestWebhook_NonPostIs405(t *testing.T) {
	pool := openWebhookTestPool(t)
	h := newTestHandler(t, pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/inbox/webhook", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestWebhook_HealthOK(t *testing.T) {
	pool := openWebhookTestPool(t)
	h := newTestHandler(t, pool)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// =============================================================================
// Mention parsing + inbox insertion
// =============================================================================

func TestWebhook_FormPost_InsertsRowForMatchedMention(t *testing.T) {
	pool := openWebhookTestPool(t)
	agentID := seedAgent(t, pool, "agent-1", "")
	h := newTestHandler(t, pool)

	postID := fmt.Sprintf("post-%d", time.Now().UnixNano())
	w := formPost(t, h, map[string]string{
		"token":        testWebhookSecret,
		"team_id":      "team-x",
		"channel_id":   "ch-x",
		"channel_name": "agent-events",
		"user_id":      "u-1",
		"user_name":    "dale",
		"post_id":      postID,
		"text":         "@agent-1 please pick this up",
		"trigger_word": "@agent-1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var (
		gotAgent  string
		gotMsg    string
		gotPostID string
	)
	err := pool.QueryRow(context.Background(),
		`SELECT target_agent_id, message, source_post_id
		   FROM mattermost_inbox
		  WHERE target_agent_id = $1`, agentID,
	).Scan(&gotAgent, &gotMsg, &gotPostID)
	if err != nil {
		t.Fatalf("select inbox row: %v", err)
	}
	if gotAgent != agentID {
		t.Fatalf("target_agent_id = %q, want %q", gotAgent, agentID)
	}
	if !strings.Contains(gotMsg, "please pick this up") {
		t.Fatalf("message = %q", gotMsg)
	}
	if gotPostID != postID {
		t.Fatalf("source_post_id = %q, want %q", gotPostID, postID)
	}
}

func TestWebhook_JSONPost_AlsoWorks(t *testing.T) {
	pool := openWebhookTestPool(t)
	agentID := seedAgent(t, pool, "agent-2", "")
	h := newTestHandler(t, pool)

	postID := fmt.Sprintf("post-json-%d", time.Now().UnixNano())
	w := jsonPost(t, h, map[string]any{
		"token":     testWebhookSecret,
		"post_id":   postID,
		"text":      "@agent-2 ping",
		"user_name": "dale",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mattermost_inbox WHERE target_agent_id = $1`, agentID,
	).Scan(&n)
	if n != 1 {
		t.Fatalf("inbox rows for agent-2 = %d, want 1", n)
	}
}

func TestWebhook_ResolvesMattermostUsername(t *testing.T) {
	pool := openWebhookTestPool(t)
	agentID := seedAgent(t, pool, "agent-operator-mac", "Splinter")
	h := newTestHandler(t, pool)

	w := formPost(t, h, map[string]string{
		"token":   testWebhookSecret,
		"post_id": fmt.Sprintf("post-splinter-%d", time.Now().UnixNano()),
		"text":    "hey @Splinter can you look at this",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mattermost_inbox WHERE target_agent_id = $1`, agentID,
	).Scan(&n)
	if n != 1 {
		t.Fatalf("inbox rows for @Splinter resolution = %d, want 1", n)
	}
}

func TestWebhook_UnknownMentionsSkippedSilently(t *testing.T) {
	pool := openWebhookTestPool(t)
	_ = seedAgent(t, pool, "agent-1", "")
	h := newTestHandler(t, pool)

	w := formPost(t, h, map[string]string{
		"token":   testWebhookSecret,
		"post_id": fmt.Sprintf("post-unknown-%d", time.Now().UnixNano()),
		"text":    "@nonexistent @also-nonexistent",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (skipped silently)", w.Code)
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mattermost_inbox`,
	).Scan(&n)
	if n != 0 {
		t.Fatalf("unknown mentions wrote rows: %d", n)
	}
}

func TestWebhook_MultipleMentionsInsertOneRowEach(t *testing.T) {
	pool := openWebhookTestPool(t)
	a1 := seedAgent(t, pool, "agent-1", "")
	a2 := seedAgent(t, pool, "agent-2", "")
	h := newTestHandler(t, pool)

	w := formPost(t, h, map[string]string{
		"token":   testWebhookSecret,
		"post_id": fmt.Sprintf("post-multi-%d", time.Now().UnixNano()),
		"text":    "@agent-1 and @agent-2 review please",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mattermost_inbox WHERE target_agent_id = ANY($1)`,
		[]string{a1, a2}).Scan(&n)
	if n != 2 {
		t.Fatalf("inbox rows = %d, want 2 (one per resolved agent)", n)
	}
}

// #45 — case-insensitive @-mention routing. All three spellings of the
// alias resolve to the same agent.
func TestWebhook_MentionResolutionIsCaseInsensitive(t *testing.T) {
	pool := openWebhookTestPool(t)
	agentID := seedAgent(t, pool, "agent-operator-mac", "Splinter")
	h := newTestHandler(t, pool)

	cases := []struct {
		name    string
		mention string
	}{
		{"lowercase", "@splinter"},
		{"uppercase", "@SPLINTER"},
		{"titlecase", "@Splinter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			postID := fmt.Sprintf("post-case-%s-%d", tc.name, time.Now().UnixNano())
			w := formPost(t, h, map[string]string{
				"token":   testWebhookSecret,
				"post_id": postID,
				"text":    "ping " + tc.mention + " can you take a look?",
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			var n int
			_ = pool.QueryRow(context.Background(),
				`SELECT count(*) FROM mattermost_inbox WHERE source_post_id = $1`, postID,
			).Scan(&n)
			if n != 1 {
				t.Fatalf("inbox rows for %s = %d, want 1 (case-insensitive resolve)", tc.mention, n)
			}
		})
	}

	// All three writes target the same agent row.
	var total int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mattermost_inbox WHERE target_agent_id = $1`, agentID,
	).Scan(&total)
	if total != 3 {
		t.Fatalf("total inbox rows for agent = %d, want 3", total)
	}
}

// #45 — duplicate-cased mentions in the same message dedupe to one insert.
func TestWebhook_DuplicateCasedMentionsDedupe(t *testing.T) {
	pool := openWebhookTestPool(t)
	agentID := seedAgent(t, pool, "agent-operator-mac", "Splinter")
	h := newTestHandler(t, pool)

	postID := fmt.Sprintf("post-dedupe-%d", time.Now().UnixNano())
	w := formPost(t, h, map[string]string{
		"token":   testWebhookSecret,
		"post_id": postID,
		"text":    "@Splinter and @SPLINTER and @splinter all you",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mattermost_inbox WHERE target_agent_id = $1`, agentID,
	).Scan(&n)
	if n != 1 {
		t.Fatalf("inbox rows = %d, want 1 (case-insensitive dedupe within a single message)", n)
	}
}

func TestWebhook_ReDeliveryIsIdempotent(t *testing.T) {
	pool := openWebhookTestPool(t)
	agentID := seedAgent(t, pool, "agent-1", "")
	h := newTestHandler(t, pool)

	postID := fmt.Sprintf("post-redeliver-%d", time.Now().UnixNano())
	fields := map[string]string{
		"token":   testWebhookSecret,
		"post_id": postID,
		"text":    "@agent-1 hello",
	}

	for i := 0; i < 3; i++ {
		w := formPost(t, h, fields)
		if w.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d", i, w.Code)
		}
	}

	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mattermost_inbox WHERE target_agent_id = $1`, agentID,
	).Scan(&n)
	if n != 1 {
		t.Fatalf("after 3 redeliveries, rows = %d, want 1 (idempotent)", n)
	}
}
