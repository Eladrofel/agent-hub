package server

// Signed join codes — the v0.4.0 federated trust path. An operator with the
// admin token AND the mint-authority token POSTs to /v1/admin/join-codes to
// mint a short-lived signed credential; a fresh agent VM redeems that
// credential at /v1/join-codes/redeem (no auth header — the code IS the
// auth) and receives its per-host bearer in the response.
//
// Wire format: `AGNT-<payload-b64url>.<sig-b64url>` where payload is JSON
// `{"jti":"<uuid>","agt":"<canonical>","exp":<unix-seconds>,"rol":"agent"}`
// and sig is HMAC-SHA256(payload, joinCodeHMACKey). All base64 is url-safe,
// no padding.
//
// Persistence: join_codes table (one row per mint, with redeemed_at as the
// single-use sentinel); kv_store table for the HMAC key + mint-authority
// secret which are generated on first boot if their env vars are unset.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Eladrofel/agent-hub/gateway/internal/store"
)

// =============================================================================
// JoinCodeStore — interface + Postgres implementation
// =============================================================================

// JoinCodeRecord is the persisted bookkeeping for a minted code.
type JoinCodeRecord struct {
	JTI                string
	AgentCanonical     string
	Alias              string
	Role               string
	ExpiresAt          time.Time
	RedeemedAt         *time.Time
	RedeemedByHostname *string
}

// JoinCodeStore is the persistence boundary for signed join codes. Insert
// is one-shot at mint; Lookup is read-on-redeem; MarkRedeemed is an atomic
// UPDATE ... WHERE redeemed_at IS NULL RETURNING so concurrent redeem
// attempts collapse cleanly to one winner + N losers (the losers see
// ErrAlreadyRedeemed → 409).
type JoinCodeStore interface {
	Insert(ctx context.Context, jti, agentCanonical, alias, role string, expiresAt time.Time) error
	Lookup(ctx context.Context, jti string) (*JoinCodeRecord, error)
	MarkRedeemed(ctx context.Context, jti, hostname string) error
}

// Sentinel errors so handlers can map to HTTP status codes cleanly.
var (
	ErrJoinCodeNotFound      = errors.New("join code not found")
	ErrJoinCodeAlreadyRedeemed = errors.New("join code already redeemed")
)

type postgresJoinCodeStore struct {
	pool *pgxpool.Pool
}

// NewPostgresJoinCodeStore returns a JoinCodeStore backed by the join_codes
// table created in migration 003.
func NewPostgresJoinCodeStore(pool *pgxpool.Pool) JoinCodeStore {
	return &postgresJoinCodeStore{pool: pool}
}

func (s *postgresJoinCodeStore) Insert(ctx context.Context, jti, agentCanonical, alias, role string, expiresAt time.Time) error {
	const q = `
		INSERT INTO join_codes (jti, agent_canonical, alias, role, expires_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5)`
	_, err := s.pool.Exec(ctx, q, jti, agentCanonical, alias, role, expiresAt)
	if err != nil {
		return fmt.Errorf("insert join_code: %w", err)
	}
	return nil
}

func (s *postgresJoinCodeStore) Lookup(ctx context.Context, jti string) (*JoinCodeRecord, error) {
	const q = `
		SELECT jti, agent_canonical, coalesce(alias, ''), role,
		       expires_at, redeemed_at, redeemed_by_hostname
		  FROM join_codes
		 WHERE jti = $1`
	var (
		rec  JoinCodeRecord
		host *string
	)
	err := s.pool.QueryRow(ctx, q, jti).Scan(
		&rec.JTI, &rec.AgentCanonical, &rec.Alias, &rec.Role,
		&rec.ExpiresAt, &rec.RedeemedAt, &host)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJoinCodeNotFound
		}
		return nil, fmt.Errorf("lookup join_code: %w", err)
	}
	rec.RedeemedByHostname = host
	return &rec, nil
}

func (s *postgresJoinCodeStore) MarkRedeemed(ctx context.Context, jti, hostname string) error {
	// Atomic: if redeemed_at is already non-null we update 0 rows and
	// return ErrJoinCodeAlreadyRedeemed. Concurrent redeemers race here
	// and exactly one wins.
	const q = `
		UPDATE join_codes
		   SET redeemed_at = now(),
		       redeemed_by_hostname = NULLIF($2, '')
		 WHERE jti = $1
		   AND redeemed_at IS NULL`
	tag, err := s.pool.Exec(ctx, q, jti, hostname)
	if err != nil {
		return fmt.Errorf("mark redeemed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJoinCodeAlreadyRedeemed
	}
	return nil
}

// =============================================================================
// Wire-format signing
// =============================================================================

// joinCodePrefix is fixed across the wire. agentctl pattern-matches on this.
const joinCodePrefix = "AGNT-"

// codePayload is the JSON shape signed into the wire-format code. Keep the
// field names short — they're emitted to the wire on every mint.
type codePayload struct {
	JTI string `json:"jti"`
	Agt string `json:"agt"`
	Exp int64  `json:"exp"`
	Rol string `json:"rol"`
}

// signJoinCode produces the AGNT-<payload>.<sig> wire format.
func signJoinCode(payload codePayload, key []byte) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	encPayload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encPayload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return joinCodePrefix + encPayload + "." + sig, nil
}

// verifyJoinCode splits + verifies the HMAC + returns the decoded payload.
// All failures collapse to a single error type so the redeem handler maps
// them uniformly to 401.
var errJoinCodeBadSig = errors.New("join code signature invalid")

func verifyJoinCode(code string, key []byte) (*codePayload, error) {
	if !strings.HasPrefix(code, joinCodePrefix) {
		return nil, errJoinCodeBadSig
	}
	body := strings.TrimPrefix(code, joinCodePrefix)
	parts := strings.SplitN(body, ".", 2)
	if len(parts) != 2 {
		return nil, errJoinCodeBadSig
	}
	encPayload, encSig := parts[0], parts[1]

	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return nil, errJoinCodeBadSig
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encPayload))
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, expected) != 1 {
		return nil, errJoinCodeBadSig
	}

	raw, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return nil, errJoinCodeBadSig
	}
	var p codePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errJoinCodeBadSig
	}
	if p.JTI == "" || p.Agt == "" || p.Exp == 0 {
		return nil, errJoinCodeBadSig
	}
	return &p, nil
}

// =============================================================================
// Bootstrap — load (or first-boot generate) the HMAC key + mint-auth token
// =============================================================================

const (
	kvKeyJoinCodeHMAC     = "join_code_hmac_key_v1"
	kvKeyMintAuthority    = "mint_authority_token_v1"
	envJoinCodeHMACKey    = "JOIN_CODE_HMAC_KEY"
	envMintAuthorityToken = "MINT_AUTHORITY_TOKEN"
)

// bootstrapJoinCodeSecrets resolves the HMAC signing key + mint-authority
// token in priority order: env var → kv_store row → generate fresh and
// persist. The generate-and-persist path logs once so the operator has a
// breadcrumb on first boot.
func bootstrapJoinCodeSecrets(ctx context.Context, st *store.Store, logger *slog.Logger) (hmacKey []byte, mintAuth string, err error) {
	hmacKey, err = loadOrGenerateBytes(ctx, st, logger,
		envJoinCodeHMACKey, kvKeyJoinCodeHMAC, "join-code HMAC key", decodeKeyFromEnv)
	if err != nil {
		return nil, "", err
	}
	mintAuthBytes, err := loadOrGenerateBytes(ctx, st, logger,
		envMintAuthorityToken, kvKeyMintAuthority, "mint-authority token",
		func(s string) ([]byte, error) { return []byte(s), nil })
	if err != nil {
		return nil, "", err
	}
	return hmacKey, string(mintAuthBytes), nil
}

// decodeKeyFromEnv accepts either hex or raw-url-base64 (32 bytes either way).
// Forgiving: tries hex first, falls back to base64-url, falls back to raw
// bytes-of-string for tests that pass short strings.
func decodeKeyFromEnv(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil && len(b) >= 16 {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) >= 16 {
		return b, nil
	}
	return nil, fmt.Errorf("JOIN_CODE_HMAC_KEY must be 16+ bytes as hex or base64url")
}

func loadOrGenerateBytes(
	ctx context.Context, st *store.Store, logger *slog.Logger,
	envName, kvKey, humanLabel string,
	decodeEnv func(string) ([]byte, error),
) ([]byte, error) {
	if raw := os.Getenv(envName); raw != "" {
		v, err := decodeEnv(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s from env %s: %w", humanLabel, envName, err)
		}
		return v, nil
	}
	v, err := kvGet(ctx, st.Pool, kvKey)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, errKVMissing) {
		return nil, fmt.Errorf("kv lookup %s: %w", humanLabel, err)
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("generate %s: %w", humanLabel, err)
	}
	if err := kvSet(ctx, st.Pool, kvKey, fresh); err != nil {
		return nil, fmt.Errorf("persist %s: %w", humanLabel, err)
	}
	logger.Info("generated "+humanLabel+" (one-time persistence)", "kv_key", kvKey)
	return fresh, nil
}

var errKVMissing = errors.New("kv_store key missing")

func kvGet(ctx context.Context, pool *pgxpool.Pool, key string) ([]byte, error) {
	var v []byte
	err := pool.QueryRow(ctx, `SELECT value FROM kv_store WHERE key = $1`, key).Scan(&v)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errKVMissing
		}
		return nil, err
	}
	return v, nil
}

func kvSet(ctx context.Context, pool *pgxpool.Pool, key string, value []byte) error {
	const q = `
		INSERT INTO kv_store (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = now()`
	_, err := pool.Exec(ctx, q, key, value)
	return err
}

// =============================================================================
// Mint-authority middleware
// =============================================================================

// requireMintAuthority enforces the second auth header on the mint endpoint:
// X-Mint-Authority: Bearer <token>. Stacked AFTER RequireAdmin so the admin
// token is still required. The two-secret design means a leaked admin token
// alone cannot mint join codes — the operator deliberately splits these.
func (a *App) requireMintAuthority(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.mintAuthorityToken == "" {
			writeError(w, http.StatusInternalServerError, "mint_authority_unset",
				"mint-authority token not configured")
			return
		}
		// Track C contract: 401 is for the primary admin-token (handled
		// by RequireAdmin upstream); 403 here means admin-token was OK
		// but the secondary X-Mint-Authority is missing or wrong.
		h := r.Header.Get("X-Mint-Authority")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			writeError(w, http.StatusForbidden, "mint_authority_missing",
				"X-Mint-Authority header missing or malformed")
			return
		}
		tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
		if subtle.ConstantTimeCompare([]byte(tok), []byte(a.mintAuthorityToken)) != 1 {
			writeError(w, http.StatusForbidden, "mint_authority_invalid",
				"mint-authority token mismatch")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hashForAudit returns a short stable identifier for a secret so we can log
// "which mint-authority issued this" without ever logging the secret itself.
func hashForAudit(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// PrintTokens dumps the persisted HMAC key (hex) + mint-authority token
// (raw) to w. Used by the `agent-hub print-tokens` subcommand so the
// operator can retrieve secrets the gateway generated on first boot. If
// the kv_store row is missing (no boot has happened yet, or the operator
// set an env-var override that bypasses persistence) the corresponding
// line reads "(set via env; not persisted)".
func PrintTokens(ctx context.Context, st *store.Store, w interface{ Write([]byte) (int, error) }) error {
	hmacKey, hmacErr := kvGet(ctx, st.Pool, kvKeyJoinCodeHMAC)
	mintAuth, mintErr := kvGet(ctx, st.Pool, kvKeyMintAuthority)

	writeLine := func(label, val string) {
		_, _ = w.Write([]byte(fmt.Sprintf("%-25s %s\n", label, val)))
	}

	if hmacErr == nil {
		writeLine("JOIN_CODE_HMAC_KEY:", hex.EncodeToString(hmacKey))
	} else if errors.Is(hmacErr, errKVMissing) {
		writeLine("JOIN_CODE_HMAC_KEY:", "(set via env; not persisted, or gateway has not booted yet)")
	} else {
		return fmt.Errorf("read HMAC key: %w", hmacErr)
	}

	if mintErr == nil {
		writeLine("MINT_AUTHORITY_TOKEN:", string(mintAuth))
	} else if errors.Is(mintErr, errKVMissing) {
		writeLine("MINT_AUTHORITY_TOKEN:", "(set via env; not persisted, or gateway has not booted yet)")
	} else {
		return fmt.Errorf("read mint-authority token: %w", mintErr)
	}
	return nil
}

// newJTI returns a fresh UUIDv4 string formatted per RFC 4122 (canonical
// hex-dash form). We generate from crypto/rand directly to avoid adding a
// dependency for a one-off use case.
func newJTI() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is a fatal-class condition; the caller would
		// fail the request anyway. Return a value that will fail later
		// validation rather than panic in library code.
		return "00000000-0000-0000-0000-000000000000"
	}
	// Set version (4) and variant (RFC 4122) bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
