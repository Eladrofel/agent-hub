// Package comms_backends defines the pluggable interface used by `agentctl
// comms-join` so future Slack/Discord backends slot in without touching the
// subcommand wiring. v0.3.0 only ships the Mattermost implementation; the
// Slack and Discord cases return a "v0.4 stub" sentinel.
//
// The interface is intentionally small — bot-user CRUD + channel join +
// scoped PAT mint. Anything outside that surface (presence, threads,
// reactions) is handled by the higher-level chat-emit / agent-inbox skills
// against the same per-VM PAT.
package comms_backends

// Backend is the minimal contract a comms backend implements for the join
// flow. All methods are network-bound; callers wrap them in best-effort vs.
// strict posture at the subcommand layer.
type Backend interface {
	// Validate ensures the admin token can hit the backend's API.
	// (For Mattermost: GET /api/v4/users/me with the admin PAT.)
	Validate(adminToken string) error

	// EnsureBotUser creates the bot user if it does not exist; returns its
	// id either way. Idempotent.
	EnsureBotUser(adminToken, name string) (botID string, err error)

	// EnsureTeamMember adds the bot to the backend's team/workspace if it
	// is not already a member. Idempotent — returning nil when already a
	// member. Bug #49: must be called before AddBotToChannel; on a fresh
	// Mattermost team the bot is not yet a team member and the
	// channel-add will fail with 403 user_not_in_team.
	EnsureTeamMember(adminToken, botID string) error

	// AddBotToChannel joins the bot to the named channel. Idempotent —
	// returning nil when already a member.
	AddBotToChannel(adminToken, botID, channel string) error

	// MintPAT mints a personal access token for the bot user and returns
	// the plaintext token. (For Mattermost: POST /api/v4/users/{id}/tokens.)
	MintPAT(adminToken, botID string) (pat string, err error)
}
