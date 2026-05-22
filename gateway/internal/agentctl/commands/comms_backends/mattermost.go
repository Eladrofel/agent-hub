package comms_backends

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Mattermost implements Backend against a Mattermost v4 REST API.
//
// Field BaseURL is the scheme+host of the Mattermost server (no path), e.g.
// "https://mattermost.example.com". TeamName is the canonical team handle
// (the URL fragment, not the display name); required for channel-name
// resolution.
//
// SkipTLSVerify mirrors the agent-hub outbox-worker's MATTERMOST_TLS_SKIP_VERIFY
// knob — required for homelab self-signed cert deployments.
type Mattermost struct {
	BaseURL       string
	TeamName      string
	SkipTLSVerify bool
}

// NewMattermost reads CONCEPT_CHAT_MM_URL + MATTERMOST_TEAM_NAME +
// MATTERMOST_TLS_SKIP_VERIFY from the environment and returns a configured
// Mattermost backend. Returns an error if the required vars are missing.
func NewMattermost() (*Mattermost, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("CONCEPT_CHAT_MM_URL")), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("env CONCEPT_CHAT_MM_URL is required")
	}
	team := strings.TrimSpace(os.Getenv("MATTERMOST_TEAM_NAME"))
	if team == "" {
		return nil, fmt.Errorf("env MATTERMOST_TEAM_NAME is required")
	}
	skip := strings.EqualFold(strings.TrimSpace(os.Getenv("MATTERMOST_TLS_SKIP_VERIFY")), "true")
	return &Mattermost{
		BaseURL:       baseURL,
		TeamName:      team,
		SkipTLSVerify: skip,
	}, nil
}

// httpClient returns an http.Client honouring SkipTLSVerify.
func (m *Mattermost) httpClient() *http.Client {
	tr := &http.Transport{}
	if m.SkipTLSVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator-toggled for homelab
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

func (m *Mattermost) do(method, path, adminToken string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, m.BaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+adminToken)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// Validate hits GET /api/v4/users/me and confirms 2xx.
func (m *Mattermost) Validate(adminToken string) error {
	status, body, err := m.do("GET", "/api/v4/users/me", adminToken, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("validate /users/me HTTP %d: %s", status, string(body))
	}
	return nil
}

// EnsureBotUser looks up the bot by username; if absent, creates it via
// POST /api/v4/bots. Returns the bot's user_id.
func (m *Mattermost) EnsureBotUser(adminToken, name string) (string, error) {
	// Lookup first: GET /api/v4/users/username/{name}
	status, body, err := m.do("GET", "/api/v4/users/username/"+url.PathEscape(name), adminToken, nil)
	if err != nil {
		return "", fmt.Errorf("lookup bot: %w", err)
	}
	if status >= 200 && status < 300 {
		var u struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &u); err != nil {
			return "", fmt.Errorf("decode user lookup: %w; body=%s", err, string(body))
		}
		if u.ID == "" {
			return "", fmt.Errorf("user lookup returned empty id; body=%s", string(body))
		}
		return u.ID, nil
	}
	if status != http.StatusNotFound {
		return "", fmt.Errorf("lookup bot HTTP %d: %s", status, string(body))
	}

	// Create. The bot POST endpoint returns a {user_id} object.
	createBody := map[string]any{
		"username":     name,
		"display_name": name,
		"description":  "agentctl-managed bot user",
	}
	status, body, err = m.do("POST", "/api/v4/bots", adminToken, createBody)
	if err != nil {
		return "", fmt.Errorf("create bot: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("create bot HTTP %d: %s", status, string(body))
	}
	var b struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return "", fmt.Errorf("decode bot create: %w; body=%s", err, string(body))
	}
	if b.UserID == "" {
		return "", fmt.Errorf("create-bot response missing user_id; body=%s", string(body))
	}
	return b.UserID, nil
}

// AddBotToChannel resolves the channel by name within m.TeamName, then POSTs
// the bot to the members endpoint. Treats "already a member" as success.
func (m *Mattermost) AddBotToChannel(adminToken, botID, channel string) error {
	// Resolve team -> id.
	status, body, err := m.do("GET", "/api/v4/teams/name/"+url.PathEscape(m.TeamName), adminToken, nil)
	if err != nil {
		return fmt.Errorf("lookup team: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("lookup team %s HTTP %d: %s", m.TeamName, status, string(body))
	}
	var t struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return fmt.Errorf("decode team lookup: %w; body=%s", err, string(body))
	}

	// Resolve channel -> id.
	status, body, err = m.do("GET", "/api/v4/teams/"+t.ID+"/channels/name/"+url.PathEscape(channel), adminToken, nil)
	if err != nil {
		return fmt.Errorf("lookup channel: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("lookup channel %s HTTP %d: %s", channel, status, string(body))
	}
	var c struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return fmt.Errorf("decode channel lookup: %w; body=%s", err, string(body))
	}

	// POST member.
	status, body, err = m.do("POST", "/api/v4/channels/"+c.ID+"/members", adminToken,
		map[string]any{"user_id": botID})
	if err != nil {
		return fmt.Errorf("add channel member: %w", err)
	}
	if status >= 200 && status < 300 {
		return nil
	}
	// "already a member" — Mattermost returns 400 with id=api.channel.add_user.to.channel.failed.deleted.app_error
	// or a 201 on re-add. Be permissive and treat 4xx with "already" message as success.
	if status >= 400 && status < 500 && bytes.Contains(bytes.ToLower(body), []byte("already")) {
		return nil
	}
	return fmt.Errorf("add channel member HTTP %d: %s", status, string(body))
}

// MintPAT calls POST /api/v4/users/{id}/tokens with a description; the
// response carries the plaintext token exactly once.
func (m *Mattermost) MintPAT(adminToken, botID string) (string, error) {
	body := map[string]any{"description": "agentctl-managed bot PAT"}
	status, raw, err := m.do("POST", "/api/v4/users/"+botID+"/tokens", adminToken, body)
	if err != nil {
		return "", fmt.Errorf("mint PAT: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("mint PAT HTTP %d: %s", status, string(raw))
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode mint-PAT: %w; body=%s", err, string(raw))
	}
	if resp.Token == "" {
		return "", fmt.Errorf("mint-PAT response missing token; body=%s", string(raw))
	}
	return resp.Token, nil
}
