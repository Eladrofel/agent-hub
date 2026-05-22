package server

// HTTP handlers for the v0.4.0 signed-join-codes federated trust path.
//
//	POST /v1/admin/join-codes        — operator mints a code (dual-auth)
//	POST /v1/join-codes/redeem       — fresh VM redeems code → per-host bearer
//
// Wire shape is LOCKED across Track A (server) + Track B (agentctl client).
// Do not reshape responses without coordinating with Track B.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Eladrofel/agent-hub/gateway/internal/agents"
)

// =============================================================================
// Mint
// =============================================================================

type mintRequest struct {
	AgentCanonical string `json:"agent_canonical"`
	Alias          string `json:"alias"`
	TTLSeconds     int64  `json:"ttl_seconds"`
	IntendedRole   string `json:"intended_role"`
}

type mintResponse struct {
	Code           string    `json:"code"`
	AgentCanonical string    `json:"agent_canonical"`
	ExpiresAt      time.Time `json:"expires_at"`
	SingleUse      bool      `json:"single_use"`
}

const (
	mintTTLMin     = 300            // 5 minutes
	mintTTLMax     = 7 * 24 * 3600  // 7 days
	mintTTLDefault = 24 * 3600      // 24 hours
)

func (a *App) handleJoinCodeMint(w http.ResponseWriter, r *http.Request) {
	var req mintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.AgentCanonical == "" {
		writeError(w, http.StatusBadRequest, "agent_canonical_required",
			"agent_canonical must be non-empty")
		return
	}
	ttl := req.TTLSeconds
	if ttl == 0 {
		ttl = mintTTLDefault
	}
	if ttl < mintTTLMin || ttl > mintTTLMax {
		writeErrorWithDetails(w, http.StatusBadRequest, "invalid_ttl",
			"ttl_seconds must be between 300 and 604800",
			map[string]int64{"got": req.TTLSeconds, "min": mintTTLMin, "max": mintTTLMax})
		return
	}
	role := req.IntendedRole
	if role == "" {
		role = "agent"
	}
	if role != "agent" && role != "operator" {
		writeErrorWithDetails(w, http.StatusBadRequest, "invalid_role",
			"intended_role must be agent or operator",
			map[string]string{"got": req.IntendedRole})
		return
	}

	jti := newJTI()
	expiresAt := time.Now().UTC().Add(time.Duration(ttl) * time.Second)

	if err := a.JoinCodes.Insert(r.Context(), jti, req.AgentCanonical, req.Alias, role, expiresAt); err != nil {
		a.Logger.Error("join-code insert failed", "err", err, "jti", jti)
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}

	code, err := signJoinCode(codePayload{
		JTI: jti,
		Agt: req.AgentCanonical,
		Exp: expiresAt.Unix(),
		Rol: role,
	}, a.joinCodeHMACKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sign_failed", err.Error())
		return
	}

	// Audit log: jti + agent + alias + a short hash of the mint-authority
	// token (NOT the token itself). Operators trace who minted what without
	// exposing the secret.
	mintAuth := r.Header.Get("X-Mint-Authority")
	a.Logger.Info("join-code minted",
		"jti", jti,
		"agent_canonical", req.AgentCanonical,
		"alias", req.Alias,
		"role", role,
		"expires_at", expiresAt,
		"mint_authority_fp", hashForAudit(mintAuth))

	writeJSON(w, http.StatusCreated, mintResponse{
		Code:           code,
		AgentCanonical: req.AgentCanonical,
		ExpiresAt:      expiresAt,
		SingleUse:      true,
	})
}

// =============================================================================
// Redeem
// =============================================================================

type redeemRequest struct {
	Code      string `json:"code"`
	Hostname  string `json:"hostname"`
	SSHPubkey string `json:"ssh_pubkey,omitempty"`
}

type redeemResponse struct {
	AgentCanonical string    `json:"agent_canonical"`
	Alias          string    `json:"alias,omitempty"`
	Role           string    `json:"role"`
	MintToken      string    `json:"mint_token"`
	GatewayURL     string    `json:"gateway_url,omitempty"`
	RegisteredAt   time.Time `json:"registered_at"`
}

func (a *App) handleJoinCodeRedeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code_required", "code must be non-empty")
		return
	}

	// Verify signature FIRST — a bad sig short-circuits before any DB hit.
	payload, err := verifyJoinCode(req.Code, a.joinCodeHMACKey)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_signature",
			"join code signature invalid")
		return
	}

	// Lookup persisted bookkeeping. 404 if missing — covers both
	// never-minted and post-expiry-cleanup deletion.
	rec, err := a.JoinCodes.Lookup(r.Context(), payload.JTI)
	if err != nil {
		if errors.Is(err, ErrJoinCodeNotFound) {
			writeError(w, http.StatusNotFound, "code_not_found",
				"join code not found")
			return
		}
		a.Logger.Error("join-code lookup failed", "err", err, "jti", payload.JTI)
		writeError(w, http.StatusInternalServerError, "lookup_failed", err.Error())
		return
	}

	// Expiry check uses the persisted expires_at (the wire-format exp is
	// the same value but the DB is authoritative — covers the edge case
	// where an operator might shorten expiry via direct DB update).
	if time.Now().After(rec.ExpiresAt) {
		writeError(w, http.StatusGone, "code_expired",
			"join code has expired")
		return
	}

	// Atomic single-use enforcement. 0-row UPDATE → already redeemed.
	if err := a.JoinCodes.MarkRedeemed(r.Context(), payload.JTI, req.Hostname); err != nil {
		if errors.Is(err, ErrJoinCodeAlreadyRedeemed) {
			writeError(w, http.StatusConflict, "code_already_redeemed",
				"join code has already been redeemed")
			return
		}
		writeError(w, http.StatusInternalServerError, "redeem_failed", err.Error())
		return
	}

	// Mint the per-host bearer the new agent will use for all future
	// /v1/* calls. This mirrors handleMintToken (admin endpoint) but is
	// reached via the trusted-code path instead of an admin token.
	plain, err := generateAgentToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bcrypt_failed", err.Error())
		return
	}
	if _, err := agents.MintToken(r.Context(), a.Store.Pool, rec.AgentCanonical, string(hash)); err != nil {
		a.Logger.Error("agent mint via redeem failed", "err", err,
			"agent_canonical", rec.AgentCanonical, "jti", payload.JTI)
		writeError(w, http.StatusInternalServerError, "agent_mint_failed", err.Error())
		return
	}

	a.Logger.Info("join-code redeemed",
		"jti", payload.JTI,
		"agent_canonical", rec.AgentCanonical,
		"hostname", req.Hostname,
		"role", rec.Role)

	writeJSON(w, http.StatusOK, redeemResponse{
		AgentCanonical: rec.AgentCanonical,
		Alias:          rec.Alias,
		Role:           rec.Role,
		MintToken:      plain,
		GatewayURL:     a.GatewayURL,
		RegisteredAt:   time.Now().UTC(),
	})
}

// generateAgentToken is the same 32-byte raw-url-base64 shape handleMintToken
// produces, factored so the redeem path uses identical token-issuance logic.
func generateAgentToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
