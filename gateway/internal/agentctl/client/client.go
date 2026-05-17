// Package client is the thin HTTP wrapper agentctl uses to talk to the
// gateway. It centralises bearer-auth, JSON encode/decode, timeouts, and
// error-envelope parsing so each subcommand stays a few lines long.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// Sentinel errors callers compare against with errors.Is.
var (
	// ErrSanitiserBlocked is returned when the gateway responds 422 with
	// error="sanitiser_blocked".
	ErrSanitiserBlocked = errors.New("sanitiser blocked")
	// ErrUnauthorized is returned for 401/403 responses.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrNotFound is returned for 404 responses.
	ErrNotFound = errors.New("not found")
	// ErrServerUnavailable is returned for 5xx responses or network errors.
	ErrServerUnavailable = errors.New("server unavailable")
	// ErrBadRequest is returned for non-sanitiser 4xx responses.
	ErrBadRequest = errors.New("bad request")
)

// ErrorEnvelope mirrors gateway/internal/server.errorResponse. Fields are
// optional so partial gateway responses still parse.
type ErrorEnvelope struct {
	Error   string          `json:"error"`
	Message string          `json:"message,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// APIError wraps a non-2xx gateway response. Implements errors.Is against
// the sentinel set so callers can switch on error type without poking at
// HTTP status codes.
type APIError struct {
	HTTPStatus int
	Envelope   ErrorEnvelope
	RawBody    []byte
}

func (e *APIError) Error() string {
	if e.Envelope.Error != "" {
		return fmt.Sprintf("HTTP %d: %s: %s", e.HTTPStatus, e.Envelope.Error, e.Envelope.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.HTTPStatus, string(e.RawBody))
}

func (e *APIError) Is(target error) bool {
	switch target {
	case ErrSanitiserBlocked:
		return e.HTTPStatus == http.StatusUnprocessableEntity && e.Envelope.Error == "sanitiser_blocked"
	case ErrUnauthorized:
		return e.HTTPStatus == http.StatusUnauthorized || e.HTTPStatus == http.StatusForbidden
	case ErrNotFound:
		return e.HTTPStatus == http.StatusNotFound
	case ErrServerUnavailable:
		return e.HTTPStatus >= 500
	case ErrBadRequest:
		return e.HTTPStatus >= 400 && e.HTTPStatus < 500
	}
	return false
}

// Client is a configured HTTP client bound to one gateway URL + token.
type Client struct {
	cfg  *config.Config
	http *http.Client
}

// New builds a Client with sane timeouts. 5s connect, 30s overall — generous
// enough for a busy gateway, short enough that a wedged caller still exits
// before a Claude tool-call timeout.
func New(cfg *config.Config) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

// Do sends one request. body may be nil. Returns (status, body, err).
//
// On a non-2xx response, returns an *APIError that satisfies errors.Is for
// the sentinel set above. On a network error, returns ErrServerUnavailable
// wrapped via fmt.Errorf so callers can detect both classes uniformly.
func (c *Client) Do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}

	url := c.cfg.URL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrServerUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, respBody, nil
	}

	// Non-2xx: try to decode the error envelope. If decode fails, the raw
	// body is preserved in APIError so callers can still surface it.
	var env ErrorEnvelope
	_ = json.Unmarshal(respBody, &env)
	return resp.StatusCode, respBody, &APIError{
		HTTPStatus: resp.StatusCode,
		Envelope:   env,
		RawBody:    respBody,
	}
}
