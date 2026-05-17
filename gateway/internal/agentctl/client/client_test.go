package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// newTestServer + newClient compose a Client pointed at an httptest server
// authenticated with token "test-token". Each test gets its own server.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		URL:   srv.URL,
		Token: "test-token",
	}
	return srv, New(cfg)
}

func TestDo_SendsBearerAndJSONBody(t *testing.T) {
	var (
		gotAuth        string
		gotContentType string
		gotBody        string
		gotMethod      string
		gotPath        string
	)
	_, cl := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"event_id":"abc"}`))
	})

	status, body, err := cl.Do(context.Background(), "POST", "/v1/events", map[string]any{"event_type": "x"})
	if err != nil {
		t.Fatalf("Do err: %v", err)
	}
	if status != 201 {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotMethod != "POST" || gotPath != "/v1/events" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"event_type":"x"`) {
		t.Fatalf("body = %q", gotBody)
	}
	var resp map[string]any
	_ = json.Unmarshal(body, &resp)
	if resp["event_id"] != "abc" {
		t.Fatalf("decoded body = %v", resp)
	}
}

func TestDo_OmitsAuthHeaderWhenTokenEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := &config.Config{URL: srv.URL}
	cl := New(cfg)
	_, _, err := cl.Do(context.Background(), "GET", "/health", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("auth = %q, want empty", gotAuth)
	}
}

func TestDo_SanitiserBlockedReturnsAPIError(t *testing.T) {
	_, cl := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"sanitiser_blocked","message":"blocked"}`))
	})

	_, _, err := cl.Do(context.Background(), "POST", "/v1/events", map[string]any{"event_type": "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrSanitiserBlocked) {
		t.Fatalf("err %v does not match ErrSanitiserBlocked", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("err should unwrap to *APIError")
	}
	if apiErr.HTTPStatus != 422 {
		t.Fatalf("status = %d", apiErr.HTTPStatus)
	}
}

func TestDo_401ReturnsErrUnauthorized(t *testing.T) {
	_, cl := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"no token"}`))
	})
	_, _, err := cl.Do(context.Background(), "GET", "/v1/events", nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err %v not ErrUnauthorized", err)
	}
}

func TestDo_500ReturnsErrServerUnavailable(t *testing.T) {
	_, cl := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"insert_failed"}`))
	})
	_, _, err := cl.Do(context.Background(), "POST", "/v1/events", map[string]any{})
	if !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("err %v not ErrServerUnavailable", err)
	}
}

func TestDo_NetworkErrorMapsToErrServerUnavailable(t *testing.T) {
	cfg := &config.Config{
		URL:   "http://127.0.0.1:1", // closed port
		Token: "tok",
	}
	cl := New(cfg)
	_, _, err := cl.Do(context.Background(), "GET", "/health", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("err %v not ErrServerUnavailable", err)
	}
}

func TestDo_2xxReturnsBody(t *testing.T) {
	_, cl := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	_, body, err := cl.Do(context.Background(), "GET", "/health", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("body = %q", string(body))
	}
}

func TestAPIError_Error_FormatsMessage(t *testing.T) {
	e := &APIError{
		HTTPStatus: 422,
		Envelope:   ErrorEnvelope{Error: "x", Message: "y"},
	}
	if !strings.Contains(e.Error(), "422") {
		t.Fatalf("error = %q", e.Error())
	}
}
