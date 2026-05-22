// Webhook receiver for Mattermost outgoing webhooks.
//
// Mattermost is configured (via the admin's outgoing-webhook UI) to fire a
// POST to `${INBOX_WEBHOOK_URL}/v1/inbox/webhook` whenever a message
// matching a trigger appears in the agent-events channel. The receiver:
//
//  1. Validates the `token` field against WEBHOOK_SECRET (constant-time
//     compare). Mismatch → 401, no body.
//  2. Parses @-mentions from the `text` field.
//  3. For each mention, resolves the handle to an agent (first by name;
//     fallback to mattermost_username).
//  4. Inserts one mattermost_inbox row per resolved agent. UNIQUE on
//     (source_post_id, target_agent_id) makes redeliveries idempotent.
//
// Body shape: Mattermost outgoing webhooks send
// application/x-www-form-urlencoded by default; if the operator switched
// to "Content type: application/json" in the webhook config, the receiver
// transparently handles that too.
package inbox

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// WebhookConfig is the runtime configuration for `agent-hub inbox-webhook`.
type WebhookConfig struct {
	ListenAddr    string // LISTEN_ADDR, default :8788
	DatabaseURL   string // DATABASE_URL
	WebhookSecret string // WEBHOOK_SECRET (shared with Mattermost outgoing webhook)
}

// DefaultWebhookConfig pulls a WebhookConfig from process env with the
// production defaults filled in.
func DefaultWebhookConfig() WebhookConfig {
	return WebhookConfig{
		ListenAddr:    envOr("LISTEN_ADDR", ":8788"),
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// RunWebhook boots the receiver and blocks until ctx is cancelled.
func RunWebhook(ctx context.Context, cfg WebhookConfig) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	if cfg.WebhookSecret == "" {
		return errors.New("WEBHOOK_SECRET is required")
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	h := NewWebhookHandler(st.Pool, cfg.WebhookSecret, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.health)
	mux.HandleFunc("/v1/inbox/webhook", h.serve)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("inbox-webhook listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// =============================================================================
// Handler — testable. NewWebhookHandler wires it; tests mount it on httptest.
// =============================================================================

// Handler is the public handler type. Use NewWebhookHandler.
type Handler struct {
	pool   *pgxpool.Pool
	secret string
	logger *slog.Logger
}

func NewWebhookHandler(pool *pgxpool.Pool, secret string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Handler{pool: pool, secret: secret, logger: logger}
}

// Routes returns an http.Handler exposing /health and /v1/inbox/webhook.
// Tests use this to mount the handler under httptest.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.health)
	mux.HandleFunc("/v1/inbox/webhook", h.serve)
	return mux
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// mentionRe matches @-handles. Captures the handle without the leading @.
// Allows letters, digits, underscore, dot, hyphen — covers "agent-1",
// "Splinter", "agent.operator", "agent-operator-mac".
var mentionRe = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9._-]*)`)

// parseInt64 returns the int64 value of s, or 0 if s is empty/unparseable.
// Used for form-encoded numeric fields where best-effort parsing is fine.
func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// webhookPayload is the subset of Mattermost's outgoing-webhook body we use.
type webhookPayload struct {
	Token       string `json:"token"`
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Timestamp   int64  `json:"timestamp"` // Mattermost sends Unix ms as a JSON number, not a string
	UserID      string `json:"user_id"`
	UserName    string `json:"user_name"`
	PostID      string `json:"post_id"`
	Text        string `json:"text"`
	TriggerWord string `json:"trigger_word"`
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	p, err := parseWebhookPayload(r)
	if err != nil {
		h.logger.Warn("webhook parse failed", "err", err)
		http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
		return
	}

	// Constant-time secret compare to defeat timing-leak attacks on the
	// shared secret. Mismatch returns 401 with no body so we don't echo
	// hints back to an attacker probing.
	if subtle.ConstantTimeCompare([]byte(p.Token), []byte(h.secret)) != 1 {
		h.logger.Warn("webhook token mismatch", "from_user", p.UserName)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	mentions := extractMentions(p.Text)
	if len(mentions) == 0 {
		// No mentions → nothing to enqueue. Still 200 so Mattermost doesn't
		// flag the integration as broken.
		writeOK(w)
		return
	}

	// Build the props blob from everything except the message body
	// (already stored separately) so the agent can see channel/team
	// metadata when polling.
	props := map[string]any{
		"team_id":      p.TeamID,
		"channel_name": p.ChannelName,
		"timestamp":    p.Timestamp,
		"user_id":      p.UserID,
		"trigger_word": p.TriggerWord,
	}
	propsJSON, _ := json.Marshal(props)

	inserted := 0
	for _, handle := range mentions {
		agentID, ok := h.resolveAgent(r.Context(), handle)
		if !ok {
			h.logger.Warn("@-mention did not resolve to a known agent", "handle", handle)
			continue
		}
		ok, err := h.insertInboxRow(r.Context(), agentID, p, propsJSON)
		if err != nil {
			h.logger.Error("inbox insert failed",
				"target_agent", agentID, "post_id", p.PostID, "err", err)
			continue
		}
		if ok {
			inserted++
		}
	}
	h.logger.Info("inbox webhook handled",
		"post_id", p.PostID, "mentions", len(mentions), "inserted", inserted)
	writeOK(w)
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

// parseWebhookPayload reads the request body as either form-encoded or JSON
// depending on Content-Type.
func parseWebhookPayload(r *http.Request) (*webhookPayload, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var p webhookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return nil, fmt.Errorf("decode json: %w", err)
		}
		return &p, nil
	}
	// Fall back to form (Mattermost's default).
	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("parse form: %w", err)
	}
	return &webhookPayload{
		Token:       r.FormValue("token"),
		TeamID:      r.FormValue("team_id"),
		ChannelID:   r.FormValue("channel_id"),
		ChannelName: r.FormValue("channel_name"),
		Timestamp:   parseInt64(r.FormValue("timestamp")), // form-encoded: best-effort numeric parse; 0 if unparseable
		UserID:      r.FormValue("user_id"),
		UserName:    r.FormValue("user_name"),
		PostID:      r.FormValue("post_id"),
		Text:        r.FormValue("text"),
		TriggerWord: r.FormValue("trigger_word"),
	}, nil
}

// extractMentions parses @-handles out of the message body. Returns the
// list of unique handles in first-seen order (case-PRESERVING — the original
// spelling is kept for forensics/audit, while resolveAgent does
// case-insensitive matching against agents.name and mattermost_username).
// Dedupe is also case-insensitive so "@Splinter" and "@splinter" appearing
// in the same message only enqueue once.
func extractMentions(text string) []string {
	if text == "" {
		return nil
	}
	matches := mentionRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, m := range matches {
		h := m[1]
		key := strings.ToLower(h)
		if !seen[key] {
			seen[key] = true
			out = append(out, h) // preserve original case
		}
	}
	return out
}

// resolveAgent maps a mention handle to an agents.id. Case-INSENSITIVE: first
// tries to match agents.name (e.g., "agent-1"), then falls back to
// mattermost_username (e.g., "@Splinter" → agent-operator-mac). The lookup
// uses LOWER(...) on both sides so "@SPLINTER" / "@splinter" / "@Splinter"
// all resolve to the same row. Returns ("", false) if neither matches.
//
// Closes #45 (v0.1.7): operators were getting silently-dropped mentions
// when case didn't match exactly.
func (h *Handler) resolveAgent(ctx context.Context, handle string) (string, bool) {
	var id string
	err := h.pool.QueryRow(ctx,
		`SELECT id FROM agents WHERE LOWER(name) = LOWER($1)`, handle).Scan(&id)
	if err == nil {
		return id, true
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		h.logger.Warn("agent name lookup failed", "handle", handle, "err", err)
		return "", false
	}
	// Try mattermost_username.
	err = h.pool.QueryRow(ctx,
		`SELECT id FROM agents WHERE LOWER(mattermost_username) = LOWER($1)`, handle).Scan(&id)
	if err == nil {
		return id, true
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		h.logger.Warn("agent mattermost_username lookup failed", "handle", handle, "err", err)
	}
	return "", false
}

// insertInboxRow writes one mattermost_inbox row. Uses ON CONFLICT DO
// NOTHING on the (source_post_id, target_agent_id) unique index so
// Mattermost's at-least-once delivery doesn't create duplicates. Returns
// (inserted, err) — inserted=false on duplicate, no error.
func (h *Handler) insertInboxRow(ctx context.Context, agentID string, p *webhookPayload, propsJSON []byte) (bool, error) {
	const q = `
		INSERT INTO mattermost_inbox (
			target_agent_id, source_username, source_channel_id,
			source_post_id, message, props
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (source_post_id, target_agent_id)
		   WHERE source_post_id IS NOT NULL
		   DO NOTHING
		RETURNING id`
	var id string
	err := h.pool.QueryRow(ctx, q,
		agentID,
		nullIfEmpty(p.UserName),
		nullIfEmpty(p.ChannelID),
		nullIfEmpty(p.PostID),
		p.Text,
		propsJSON,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT swallowed the insert — that's the idempotent path.
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
