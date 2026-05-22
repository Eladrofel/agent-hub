-- 003_join_codes_kv.sql
-- v0.4.0 federated trust path: signed join codes + a small kv_store for
-- gateway-local secrets (HMAC signing key, mint-authority token).
--
-- kv_store: opaque per-key bytes. Used by the gateway to persist
-- secrets generated on first boot so they survive container restarts.
-- Values are NOT encrypted at rest — the operator is expected to protect
-- the Postgres volume the same way they'd protect any other backend
-- secret store.
--
-- join_codes: one row per minted code. The wire-format code carries the
-- signed payload; this table records the bookkeeping (expiry, redeemed
-- state) needed to enforce single-use semantics and answer 410/409.
-- redeemed_at IS NULL acts as the "still claimable" sentinel; the redeem
-- handler atomically UPDATEs that row and treats a 0-row UPDATE as 409
-- (already redeemed).

CREATE TABLE IF NOT EXISTS kv_store (
  key text PRIMARY KEY,
  value bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS join_codes (
  jti uuid PRIMARY KEY,
  agent_canonical text NOT NULL,
  alias text,
  role text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  redeemed_at timestamptz,
  redeemed_by_hostname text
);

CREATE INDEX IF NOT EXISTS idx_join_codes_unredeemed
ON join_codes(expires_at)
WHERE redeemed_at IS NULL;
