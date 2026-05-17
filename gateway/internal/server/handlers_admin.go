package server

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/Eladrofel/terraform-agent-hub/gateway/internal/agents"
)

// mintTokenResponse: the plaintext token is returned exactly once. The
// caller (typically /setup-agent-events) is responsible for writing it to
// the agent's per-host token file before exiting.
type mintTokenResponse struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	Token   string `json:"token"`
}

// handleMintToken upserts the agents row, generates a fresh 32-byte
// base64-url token, bcrypts it into token_hash, and returns the plaintext
// (the only time it's visible). Gated by ADMIN_TOKEN.
func (a *App) handleMintToken(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name_required", "missing path parameter")
		return
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	plain := base64.RawURLEncoding.EncodeToString(tokenBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bcrypt_failed", err.Error())
		return
	}

	id, err := agents.MintToken(r.Context(), a.Store.Pool, name, string(hash))
	if err != nil {
		a.Logger.Error("mint token failed", "name", name, "err", err)
		writeError(w, http.StatusInternalServerError, "mint_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, mintTokenResponse{
		AgentID: id,
		Name:    name,
		Token:   plain,
	})
}
