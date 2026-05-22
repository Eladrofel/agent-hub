package server

// Integration tests for the signed-join-codes mint + redeem flow.
// Requires AGENT_HUB_TEST_DATABASE_URL — skipped otherwise, same pattern
// as server_test.go.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Eladrofel/agent-hub/gateway/internal/auth"
	"github.com/Eladrofel/agent-hub/gateway/internal/sanitiser"
	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

const testMintAuthority = "test-mint-authority-secret"

type joinTestEnv struct {
	t       *testing.T
	ctx     context.Context
	store   *store.Store
	app     *App
	handler http.Handler
}

func newJoinTestEnv(t *testing.T) *joinTestEnv {
	t.Helper()
	dsn := os.Getenv("AGENT_HUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_HUB_TEST_DATABASE_URL not set; skipping integration tests")
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
	truncateAll(t, st)
	// truncateAll doesn't know about join_codes/kv_store; clean them too.
	_, _ = st.Pool.Exec(ctx, "TRUNCATE TABLE join_codes")
	_, _ = st.Pool.Exec(ctx, "TRUNCATE TABLE kv_store")

	san, err := sanitiser.Load("/dev/null", nil)
	if err != nil {
		// /dev/null reads as empty — good. If the loader rejects it,
		// fall back to a tempfile.
		f, _ := os.CreateTemp(t.TempDir(), "patterns-*")
		_ = f.Close()
		san, err = sanitiser.Load(f.Name(), nil)
		if err != nil {
			t.Fatalf("sanitiser: %v", err)
		}
	}

	mw := &auth.Middleware{Pool: st.Pool, AdminToken: "test-admin"}

	hmacKey := newTestHMACKey(t)
	app := &App{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:              st,
		Sanitiser:          san,
		Auth:               mw,
		StartedAt:          time.Now().UTC(),
		Version:            "test",
		JoinCodes:          NewPostgresJoinCodeStore(st.Pool),
		joinCodeHMACKey:    hmacKey,
		mintAuthorityToken: testMintAuthority,
		GatewayURL:         "https://test.example.com",
	}
	r := NewRouter(app, nil)

	return &joinTestEnv{t: t, ctx: ctx, store: st, app: app, handler: r}
}

func (e *joinTestEnv) mint(t *testing.T, adminTok, mintAuthTok string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/join-codes", bytesReader(buf))
	r.Header.Set("Content-Type", "application/json")
	if adminTok != "" {
		r.Header.Set("Authorization", "Bearer "+adminTok)
	}
	if mintAuthTok != "" {
		r.Header.Set("X-Mint-Authority", "Bearer "+mintAuthTok)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

func (e *joinTestEnv) redeem(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/join-codes/redeem", bytesReader(buf))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

func bytesReader(b []byte) *bytesReadCloser { return &bytesReadCloser{b: b} }

type bytesReadCloser struct {
	b []byte
	i int
}

func (b *bytesReadCloser) Read(p []byte) (int, error) {
	if b.i >= len(b.b) {
		return 0, io.EOF
	}
	n := copy(p, b.b[b.i:])
	b.i += n
	return n, nil
}
func (b *bytesReadCloser) Close() error { return nil }

// =============================================================================
// Mint
// =============================================================================

func TestJoinCode_Mint_WrongMintAuthority_403(t *testing.T) {
	env := newJoinTestEnv(t)
	w := env.mint(t, "test-admin", "WRONG-mint-authority", mintRequest{
		AgentCanonical: "agent-3", TTLSeconds: 600, IntendedRole: "agent",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestJoinCode_Mint_MissingMintAuthority_403(t *testing.T) {
	env := newJoinTestEnv(t)
	w := env.mint(t, "test-admin", "", mintRequest{
		AgentCanonical: "agent-3",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestJoinCode_Mint_WrongAdminToken_401(t *testing.T) {
	env := newJoinTestEnv(t)
	w := env.mint(t, "WRONG-admin", testMintAuthority, mintRequest{
		AgentCanonical: "agent-3",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

func TestJoinCode_Mint_HappyPath_201(t *testing.T) {
	env := newJoinTestEnv(t)
	w := env.mint(t, "test-admin", testMintAuthority, mintRequest{
		AgentCanonical: "agent-3", Alias: "Raph", TTLSeconds: 3600, IntendedRole: "agent",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp mintResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AgentCanonical != "agent-3" || !resp.SingleUse {
		t.Errorf("bad resp: %+v", resp)
	}
	// Verify the code parses back + HMAC matches.
	payload, err := verifyJoinCode(resp.Code, env.app.joinCodeHMACKey)
	if err != nil {
		t.Fatalf("returned code does not verify: %v", err)
	}
	if payload.Agt != "agent-3" || payload.Rol != "agent" {
		t.Errorf("decoded payload mismatch: %+v", payload)
	}
}

func TestJoinCode_Mint_InvalidTTL_400(t *testing.T) {
	env := newJoinTestEnv(t)
	w := env.mint(t, "test-admin", testMintAuthority, mintRequest{
		AgentCanonical: "agent-3", TTLSeconds: 10, // too small
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// =============================================================================
// Redeem
// =============================================================================

func mustMint(t *testing.T, env *joinTestEnv, agent string) string {
	t.Helper()
	w := env.mint(t, "test-admin", testMintAuthority, mintRequest{
		AgentCanonical: agent, Alias: "alias-" + agent, TTLSeconds: 3600, IntendedRole: "agent",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("mint failed: %d %s", w.Code, w.Body.String())
	}
	var resp mintResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Code
}

func TestJoinCode_Redeem_HappyPath_200(t *testing.T) {
	env := newJoinTestEnv(t)
	code := mustMint(t, env, "agent-7")

	w := env.redeem(t, redeemRequest{Code: code, Hostname: "claude-7"})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp redeemResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AgentCanonical != "agent-7" || resp.Role != "agent" || resp.MintToken == "" {
		t.Errorf("bad resp: %+v", resp)
	}
	if resp.GatewayURL != "https://test.example.com" {
		t.Errorf("gateway_url = %q, want test.example.com", resp.GatewayURL)
	}

	// Agent row should exist with the bcrypt'd token, joinable via the
	// minted-token plaintext.
	var n int
	_ = env.store.Pool.QueryRow(env.ctx,
		`SELECT count(*) FROM agents WHERE name = 'agent-7' AND token_hash IS NOT NULL`,
	).Scan(&n)
	if n != 1 {
		t.Errorf("agents row count = %d, want 1", n)
	}
}

func TestJoinCode_Redeem_Twice_200_then_409(t *testing.T) {
	env := newJoinTestEnv(t)
	code := mustMint(t, env, "agent-8")

	w1 := env.redeem(t, redeemRequest{Code: code, Hostname: "claude-8"})
	if w1.Code != http.StatusOK {
		t.Fatalf("first redeem: %d %s", w1.Code, w1.Body.String())
	}
	w2 := env.redeem(t, redeemRequest{Code: code, Hostname: "claude-8b"})
	if w2.Code != http.StatusConflict {
		t.Fatalf("second redeem: %d, want 409; body=%s", w2.Code, w2.Body.String())
	}
}

func TestJoinCode_Redeem_Expired_410(t *testing.T) {
	env := newJoinTestEnv(t)
	// Mint with the normal flow then backdate expires_at to simulate
	// expiry without sleeping.
	code := mustMint(t, env, "agent-9")
	payload, _ := verifyJoinCode(code, env.app.joinCodeHMACKey)
	_, err := env.store.Pool.Exec(env.ctx,
		`UPDATE join_codes SET expires_at = now() - interval '1 minute' WHERE jti = $1`,
		payload.JTI)
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}
	w := env.redeem(t, redeemRequest{Code: code, Hostname: "claude-9"})
	if w.Code != http.StatusGone {
		t.Fatalf("code = %d, want 410; body=%s", w.Code, w.Body.String())
	}
}

func TestJoinCode_Redeem_TamperedSig_401(t *testing.T) {
	env := newJoinTestEnv(t)
	code := mustMint(t, env, "agent-10")
	idx := len(code) - 4
	tampered := code[:idx] + "AAAA"
	w := env.redeem(t, redeemRequest{Code: tampered, Hostname: "claude-10"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

func TestJoinCode_Redeem_BogusJTI_404(t *testing.T) {
	env := newJoinTestEnv(t)
	// Sign a code with a fresh JTI that was never inserted.
	payload := codePayload{
		JTI: "99999999-9999-4999-8999-999999999999",
		Agt: "agent-ghost",
		Exp: time.Now().Add(time.Hour).Unix(),
		Rol: "agent",
	}
	code, err := signJoinCode(payload, env.app.joinCodeHMACKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	w := env.redeem(t, redeemRequest{Code: code, Hostname: "claude-ghost"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestJoinCode_Redeem_NoBody_400(t *testing.T) {
	env := newJoinTestEnv(t)
	w := env.redeem(t, redeemRequest{Hostname: "claude-x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
