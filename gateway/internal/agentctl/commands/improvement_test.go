package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// improvement emit — payload-shape + validation tests (v0.1.9)
//
// All tests reuse the testFixture from commands_test.go (same package).
// =============================================================================

func TestImprovementEmit_BuildsCorrectRequest(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-imp-uuid"}`

	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "process",
		"--summary", "Discovered outgoing-webhook quirk during smoke",
		"--context", "v0.1.7 deploy smoke",
		"--propagation", "mm",
		"--details", "longer body for the durable row",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotPath != "/v1/events" {
		t.Fatalf("path = %s, want /v1/events", f.gotPath)
	}
	if f.gotBody["event_type"] != "agent.improvement-note" {
		t.Fatalf("event_type = %v", f.gotBody["event_type"])
	}
	if f.gotBody["summary"] != "Discovered outgoing-webhook quirk during smoke" {
		t.Fatalf("summary = %v", f.gotBody["summary"])
	}
	if f.gotBody["project_slug"] != "demo-project" {
		t.Fatalf("project_slug should default to AGENT_PROJECT_SLUG; got %v", f.gotBody["project_slug"])
	}
	payload, ok := f.gotBody["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not an object; raw=%s", f.gotRawBody)
	}
	if payload["category"] != "process" {
		t.Fatalf("payload.category = %v", payload["category"])
	}
	if payload["propagation_hint"] != "mm" {
		t.Fatalf("payload.propagation_hint = %v", payload["propagation_hint"])
	}
	if payload["context"] != "v0.1.7 deploy smoke" {
		t.Fatalf("payload.context = %v", payload["context"])
	}
	if payload["details"] != "longer body for the durable row" {
		t.Fatalf("payload.details = %v", payload["details"])
	}
	if payload["summary"] != "Discovered outgoing-webhook quirk during smoke" {
		t.Fatalf("payload.summary = %v", payload["summary"])
	}
	if !strings.Contains(f.stderr.String(), "improvement-note emitted (category=process, propagation=mm") {
		t.Fatalf("stderr success line missing; got %q", f.stderr.String())
	}
}

func TestImprovementEmit_DefaultPropagationIsNone(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-imp"}`

	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "tooling",
		"--summary", "minimal note",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	payload, _ := f.gotBody["payload"].(map[string]any)
	if payload["propagation_hint"] != "none" {
		t.Fatalf("default propagation should be 'none'; got %v", payload["propagation_hint"])
	}
}

func TestImprovementEmit_RejectsMissingCategory(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewImprovementCmd(), "emit",
		"--summary", "no category set",
	)
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--category is required") {
		t.Fatalf("stderr should explain category requirement; got %q", f.stderr.String())
	}
}

func TestImprovementEmit_RejectsInvalidCategory(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "bogus",
		"--summary", "x",
	)
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), `--category="bogus" invalid`) {
		t.Fatalf("stderr should reject invalid category; got %q", f.stderr.String())
	}
}

func TestImprovementEmit_RejectsInvalidPropagation(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "process",
		"--summary", "x",
		"--propagation", "loud",
	)
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), `--propagation="loud" invalid`) {
		t.Fatalf("stderr should reject invalid propagation; got %q", f.stderr.String())
	}
}

func TestImprovementEmit_RejectsEmptySummary(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "process",
		"--summary", "   ",
	)
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--summary is required") {
		t.Fatalf("stderr should reject empty summary; got %q", f.stderr.String())
	}
}

func TestImprovementEmit_RejectsOversizedSummary(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("a", improvementSummaryMaxRunes+1)
	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "process",
		"--summary", long,
	)
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "is 281 chars; max is 280") {
		t.Fatalf("stderr should report exact overage; got %q", f.stderr.String())
	}
}

func TestImprovementEmit_OversizedSummaryStrictExits1(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("a", improvementSummaryMaxRunes+1)
	err := f.runNested(NewImprovementCmd(), "emit",
		"--strict",
		"--category", "process",
		"--summary", long,
	)
	if err == nil {
		t.Fatal("strict mode should propagate validation failure")
	}
	if !strings.Contains(f.stderr.String(), "halting (--strict)") {
		t.Fatalf("stderr should mark strict halt; got %q", f.stderr.String())
	}
}

func TestImprovementEmit_DetailsAtFileReadsFromDisk(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 201
	f.responseBody = `{"event_id":"evt-imp"}`

	tmp := filepath.Join(t.TempDir(), "details.md")
	body := "## what we found\n\nIt was the DNS, as ever.\n"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp details: %v", err)
	}

	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "domain",
		"--summary", "post-mortem note",
		"--details", "@"+tmp,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	payload, _ := f.gotBody["payload"].(map[string]any)
	if payload["details"] != body {
		t.Fatalf("payload.details = %q; want %q", payload["details"], body)
	}
}

func TestImprovementEmit_DetailsAtFileMissingIsValidationError(t *testing.T) {
	f := newFixture(t)
	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "process",
		"--summary", "x",
		"--details", "@/nonexistent/path/almost-certainly-not-there",
	)
	if err != nil {
		t.Fatalf("best-effort default: validation should exit 0; got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "--details @") {
		t.Fatalf("stderr should mention the file path; got %q", f.stderr.String())
	}
}

func TestImprovementEmit_StrictPropagatesServerError(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 500
	f.responseBody = `{"error":"insert_failed","message":"db down"}`

	err := f.runNested(NewImprovementCmd(), "emit",
		"--strict",
		"--category", "tooling",
		"--summary", "test",
	)
	if err == nil {
		t.Fatal("strict + 500 should return error")
	}
}

func TestImprovementEmit_BestEffortServerErrorExits0(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 500
	f.responseBody = `{"error":"insert_failed","message":"db down"}`

	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "tooling",
		"--summary", "test",
	)
	if err != nil {
		t.Fatalf("best-effort default: server 5xx should exit 0; got %v", err)
	}
}

func TestImprovementEmit_SanitiserBlockedSurfacesPatternDetail(t *testing.T) {
	f := newFixture(t)
	f.responseStatus = 422
	f.responseBody = `{"error":"sanitiser_blocked","message":"blocked","matched_pattern":"secret-pat","matched_field":"payload.details","blocked_event_id":"audit-9"}`

	err := f.runNested(NewImprovementCmd(), "emit",
		"--category", "process",
		"--summary", "sanitiser will trip this",
	)
	if err != nil {
		t.Fatalf("best-effort default: should exit 0; got %v", err)
	}
	stderr := f.stderr.String()
	if !strings.Contains(stderr, "sanitiser blocked improvement-note") {
		t.Fatalf("stderr missing label; got %q", stderr)
	}
	if !strings.Contains(stderr, `matched_pattern="secret-pat"`) {
		t.Fatalf("stderr missing matched_pattern; got %q", stderr)
	}
	if !strings.Contains(stderr, "matched_field=payload.details") {
		t.Fatalf("stderr missing matched_field; got %q", stderr)
	}
}
