package commands

import (
	"strings"
	"testing"
)

// =============================================================================
// v0.1.10 — --intent flag wiring (event emit + improvement emit)
//
// The flag is the agentctl side of a contract jointly held with the gateway:
//
//   - Absent / empty: no payload.intent field sent; the gateway treats the
//     event as info.
//   - Valid enum value: written to payload.intent.
//   - Invalid value: validated locally before any network IO; surfaces as a
//     validation error (best-effort exit 0 / strict non-zero, matching every
//     other CLI validation path).
//   - intent=directive: agentctl does NOT pre-flight the role check; the
//     gateway is the source of truth on authorisation and returns 403
//     directive_not_authorized for non-operators. We don't duplicate the
//     check here — see handlers_events_intent_test.go on the server side.
// =============================================================================

func TestValidateIntent_AcceptsEmpty(t *testing.T) {
	if err := validateIntent(""); err != nil {
		t.Fatalf("empty intent should be allowed (treated as 'info'); got %v", err)
	}
}

func TestValidateIntent_AcceptsAllEnumValues(t *testing.T) {
	for _, v := range ValidIntents {
		if err := validateIntent(v); err != nil {
			t.Errorf("intent=%q should be valid; got %v", v, err)
		}
	}
}

func TestValidateIntent_RejectsUnknown(t *testing.T) {
	err := validateIntent("urgent")
	if err == nil {
		t.Fatal("intent=urgent should be rejected")
	}
	// Error message must list the enum so operators don't have to grep code.
	for _, v := range ValidIntents {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("error message missing enum value %q: %v", v, err)
		}
	}
}

// -----------------------------------------------------------------------------
// event emit --intent
// -----------------------------------------------------------------------------

func TestEventEmit_IntentThreadedIntoPayload(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-uuid"}`

	if err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--summary", "checkpoint reached",
		"--intent", "status",
	); err != nil {
		t.Fatalf("run: %v", err)
	}
	pl, ok := f.gotBody["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not an object; raw=%s", f.gotRawBody)
	}
	if pl["intent"] != "status" {
		t.Fatalf("payload.intent = %v, want 'status'", pl["intent"])
	}
}

func TestEventEmit_AbsentIntentOmitsField(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-uuid"}`

	if err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--summary", "no intent set",
	); err != nil {
		t.Fatalf("run: %v", err)
	}
	// payload may or may not be present; if it is, intent must not be set.
	if pl, ok := f.gotBody["payload"].(map[string]any); ok {
		if _, exists := pl["intent"]; exists {
			t.Fatalf("absent --intent should omit payload.intent; got %v", pl["intent"])
		}
	}
}

func TestEventEmit_IntentMergedWithJSONPayload(t *testing.T) {
	// --intent should add to (not replace) the JSON payload supplied by
	// --json-payload. Flag wins if the JSON also sets intent — the flag is
	// the explicit signal.
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-uuid"}`

	if err := f.runNested(NewEventCmd(), "emit",
		"--type", "task.blocked",
		"--summary", "compile broke",
		"--json-payload", `{"reason":"missing fixture","intent":"info"}`,
		"--intent", "blocker",
	); err != nil {
		t.Fatalf("run: %v", err)
	}
	pl, ok := f.gotBody["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not an object; raw=%s", f.gotRawBody)
	}
	if pl["reason"] != "missing fixture" {
		t.Errorf("payload.reason lost: %v", pl["reason"])
	}
	if pl["intent"] != "blocker" {
		t.Errorf("flag should override JSON; got intent=%v", pl["intent"])
	}
}

func TestEventEmit_InvalidIntentValidationError(t *testing.T) {
	// best-effort (default): validation returns errSilent → cobra exits 0;
	// the surface to the operator is stderr. Matches the existing
	// --category / --propagation rejection tests.
	f := newFixture(t)
	err := f.runNested(NewEventCmd(), "emit",
		"--type", "progress.updated",
		"--summary", "x",
		"--intent", "urgent",
	)
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), `--intent="urgent" invalid`) {
		t.Fatalf("stderr should reject invalid intent; got %q", f.stderr.String())
	}
	// Network call must NOT have happened — validation is pre-flight.
	if f.gotPath != "" {
		t.Errorf("validation should fail BEFORE the network call; got path=%q", f.gotPath)
	}
}

// -----------------------------------------------------------------------------
// improvement emit --intent
// -----------------------------------------------------------------------------

func TestImprovementEmit_IntentThreadedIntoPayload(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-imp"}`

	if err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "process",
		"--summary", "discovered a quirk",
		"--intent", "info",
	); err != nil {
		t.Fatalf("run: %v", err)
	}
	pl, ok := f.gotBody["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not an object; raw=%s", f.gotRawBody)
	}
	if pl["intent"] != "info" {
		t.Fatalf("payload.intent = %v, want 'info'", pl["intent"])
	}
}

func TestImprovementEmit_InvalidIntentValidationError(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "process",
		"--summary", "x",
		"--intent", "loud",
	)
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), `--intent="loud" invalid`) {
		t.Fatalf("stderr should reject invalid intent; got %q", f.stderr.String())
	}
	if f.gotPath != "" {
		t.Errorf("validation should fail BEFORE the network call; got path=%q", f.gotPath)
	}
}
