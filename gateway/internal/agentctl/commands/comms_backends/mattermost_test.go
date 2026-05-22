package comms_backends

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newMockMM stands up an httptest server emulating the subset of the MM v4 API
// the backend uses, with hook points for the team-list endpoint (#50) and the
// team-members endpoint (#49).
func newMockMM(t *testing.T, opts mockMMOpts) *Mattermost {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/teams":
			opts.teamsListHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(opts.teams)
		case strings.HasPrefix(r.URL.Path, "/api/v4/teams/name/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v4/teams/name/")
			for _, team := range opts.teams {
				if team.Name == name {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(team)
					return
				}
			}
			http.Error(w, `{"id":"team_not_found"}`, http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/members") && strings.Contains(r.URL.Path, "/teams/"):
			opts.teamMembersHits.Add(1)
			if opts.teamMemberStatus != 0 {
				w.WriteHeader(opts.teamMemberStatus)
				_, _ = w.Write([]byte(opts.teamMemberBody))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"team_id":"t1","user_id":"u1"}`))
		default:
			http.Error(w, `{"id":"unknown_endpoint","path":"`+r.URL.Path+`"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Mattermost{BaseURL: srv.URL, TeamName: opts.preconfiguredTeamName}
}

type mmTeam struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

type mockMMOpts struct {
	teams                 []mmTeam
	preconfiguredTeamName string
	teamMemberStatus      int
	teamMemberBody        string
	teamsListHits         atomic.Int32
	teamMembersHits       atomic.Int32
}

// =============================================================================
// bug #50 — auto-derive MATTERMOST_TEAM_NAME
// =============================================================================

func TestResolveTeamName_AutoDerivesSingleTeam(t *testing.T) {
	opts := &mockMMOpts{
		teams: []mmTeam{{ID: "t-only", Name: "lone-team", DisplayName: "Lone"}},
	}
	mm := newMockMM(t, *opts)
	mm.BaseURL = mm.BaseURL // keep reference for clarity
	got, err := mm.resolveTeamName("admin-tok")
	if err != nil {
		t.Fatalf("resolveTeamName: %v", err)
	}
	if got != "lone-team" {
		t.Fatalf("auto-derive should pick the only team; got %q", got)
	}
	if mm.TeamName != "lone-team" {
		t.Fatalf("auto-derived name should be cached on receiver; got %q", mm.TeamName)
	}
}

func TestResolveTeamName_PreservesExplicitName(t *testing.T) {
	// When MATTERMOST_TEAM_NAME is set, the teams API must NOT be hit.
	mm := &Mattermost{BaseURL: "http://unused.invalid", TeamName: "explicit-team"}
	got, err := mm.resolveTeamName("admin-tok")
	if err != nil {
		t.Fatalf("resolveTeamName: %v", err)
	}
	if got != "explicit-team" {
		t.Fatalf("should preserve explicit name; got %q", got)
	}
}

func TestResolveTeamName_ErrorsWhenZeroTeams(t *testing.T) {
	opts := mockMMOpts{teams: []mmTeam{}}
	mm := newMockMM(t, opts)
	_, err := mm.resolveTeamName("admin-tok")
	if err == nil {
		t.Fatal("expected error when admin sees no teams")
	}
	if !strings.Contains(err.Error(), "no teams") {
		t.Fatalf("error should mention zero teams: %v", err)
	}
}

func TestResolveTeamName_ErrorsWhenMultipleTeams(t *testing.T) {
	opts := mockMMOpts{teams: []mmTeam{
		{ID: "a", Name: "alpha", DisplayName: "Alpha"},
		{ID: "b", Name: "beta", DisplayName: "Beta"},
	}}
	mm := newMockMM(t, opts)
	_, err := mm.resolveTeamName("admin-tok")
	if err == nil {
		t.Fatal("expected error when multiple teams visible")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("error should list teams: %v", err)
	}
}

// =============================================================================
// bug #49 — EnsureTeamMember idempotency
// =============================================================================

func TestEnsureTeamMember_Success(t *testing.T) {
	opts := mockMMOpts{
		teams: []mmTeam{{ID: "t-only", Name: "lone-team", DisplayName: "Lone"}},
	}
	mm := newMockMM(t, opts)
	if err := mm.EnsureTeamMember("admin-tok", "bot-1"); err != nil {
		t.Fatalf("EnsureTeamMember: %v", err)
	}
}

func TestEnsureTeamMember_AlreadyMember_IsNoOp(t *testing.T) {
	// MM returns 4xx with "already a member"-ish body on re-add; backend
	// must treat that as success.
	opts := mockMMOpts{
		teams: []mmTeam{{ID: "t-only", Name: "lone-team", DisplayName: "Lone"}},
		teamMemberStatus: http.StatusBadRequest,
		teamMemberBody:   `{"id":"api.team.add_user.to.team.failed.error","message":"User is already a member of this team"}`,
	}
	mm := newMockMM(t, opts)
	if err := mm.EnsureTeamMember("admin-tok", "bot-1"); err != nil {
		t.Fatalf("EnsureTeamMember should treat already-member as success: %v", err)
	}
}

func TestEnsureTeamMember_OtherErrorPropagates(t *testing.T) {
	opts := mockMMOpts{
		teams:            []mmTeam{{ID: "t-only", Name: "lone-team", DisplayName: "Lone"}},
		teamMemberStatus: http.StatusForbidden,
		teamMemberBody:   `{"id":"api.team.add_user.to.team.failed.permissions","message":"forbidden"}`,
	}
	mm := newMockMM(t, opts)
	err := mm.EnsureTeamMember("admin-tok", "bot-1")
	if err == nil {
		t.Fatal("non-already 4xx must propagate as error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should mention HTTP status: %v", err)
	}
}
