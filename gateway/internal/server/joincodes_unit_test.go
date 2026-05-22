package server

// Pure-unit tests for the join-code signing/verification primitives.
// No Postgres needed; these run on every `go test ./...` invocation.

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func newTestHMACKey(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func TestSignJoinCode_RoundTrip(t *testing.T) {
	key := newTestHMACKey(t)
	payload := codePayload{
		JTI: "11111111-1111-4111-8111-111111111111",
		Agt: "agent-3",
		Exp: time.Now().Add(time.Hour).Unix(),
		Rol: "agent",
	}
	code, err := signJoinCode(payload, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.HasPrefix(code, "AGNT-") {
		t.Fatalf("code missing AGNT- prefix: %q", code)
	}
	if !strings.Contains(code, ".") {
		t.Fatalf("code missing payload/sig separator: %q", code)
	}

	got, err := verifyJoinCode(code, key)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.JTI != payload.JTI || got.Agt != payload.Agt || got.Exp != payload.Exp || got.Rol != payload.Rol {
		t.Fatalf("payload mismatch: got=%+v want=%+v", got, payload)
	}
}

func TestVerifyJoinCode_TamperedSig(t *testing.T) {
	key := newTestHMACKey(t)
	code, err := signJoinCode(codePayload{
		JTI: "22222222-2222-4222-8222-222222222222",
		Agt: "agent-3", Exp: time.Now().Add(time.Hour).Unix(), Rol: "agent",
	}, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Flip a byte in the signature half.
	idx := strings.LastIndex(code, ".") + 1
	tampered := code[:idx] + flipFirstChar(code[idx:])
	if _, err := verifyJoinCode(tampered, key); err == nil {
		t.Fatal("verify accepted tampered sig")
	}
}

func TestVerifyJoinCode_WrongKey(t *testing.T) {
	keyA := newTestHMACKey(t)
	keyB := newTestHMACKey(t)
	code, _ := signJoinCode(codePayload{
		JTI: "33333333-3333-4333-8333-333333333333",
		Agt: "agent-3", Exp: time.Now().Add(time.Hour).Unix(), Rol: "agent",
	}, keyA)
	if _, err := verifyJoinCode(code, keyB); err == nil {
		t.Fatal("verify accepted code signed with different key")
	}
}

func TestVerifyJoinCode_MalformedShapes(t *testing.T) {
	key := newTestHMACKey(t)
	cases := []string{
		"",
		"not-prefixed",
		"AGNT-no-dot",
		"AGNT-.empty-halves",
		"AGNT-!@#$.also-bad",
	}
	for _, c := range cases {
		if _, err := verifyJoinCode(c, key); err == nil {
			t.Errorf("verify accepted malformed code %q", c)
		}
	}
}

func TestNewJTI_Uniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		j := newJTI()
		if len(j) != 36 || j[8] != '-' || j[14] != '4' {
			t.Errorf("malformed jti: %q", j)
		}
		if seen[j] {
			t.Errorf("duplicate jti %q at iter %d", j, i)
		}
		seen[j] = true
	}
}

func flipFirstChar(s string) string {
	if s == "" {
		return "X"
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
