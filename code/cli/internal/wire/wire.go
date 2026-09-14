// Package wire mirrors the JSON of the tools the CLI calls. It does not import
// the backend: the contract is the tool's JSON, and the integration test
// decodes strictly so a server change these structs do not know fails there.
package wire

import (
	"encoding/json"
	"time"

	"github.com/mklfarha/metiche/cli/internal/secret"
)

type Health struct {
	OK              bool   `json:"ok"`
	ProtocolVersion string `json:"protocol_version"`
	Database        string `json:"database"`
	Authenticated   bool   `json:"authenticated"`
	ServerTime      string `json:"server_time"`
	Note            string `json:"note,omitempty"`
}

type WhoamiAgent struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	ClientKey  string `json:"client_key"`
	ClientKind string `json:"client_kind,omitempty"`
	Status     string `json:"status"`
}

type RecentRequest struct {
	UserAgent string `json:"user_agent"`
	LastAt    string `json:"last_at"`
}

type Whoami struct {
	OK                  bool            `json:"ok"`
	Authenticated       bool            `json:"authenticated"`
	TokenScope          string          `json:"token_scope"`
	AccountKey          string          `json:"account_key,omitempty"`
	Agent               *WhoamiAgent    `json:"agent"`
	Teams               int             `json:"teams"`
	RecentRequests      []RecentRequest `json:"recent_requests"`
	RecentRequestsScope string          `json:"recent_requests_scope"`
	HeadersReceived     []string        `json:"headers_received"`
	ServerTime          string          `json:"server_time"`
	ProtocolVersion     string          `json:"protocol_version"`
	Note                string          `json:"note"`
}

type TeamChoice struct {
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	Members       int    `json:"members"`
	ActiveSession bool   `json:"active_session"`
}

type ListTeams struct {
	OK    bool         `json:"ok"`
	Teams []TeamChoice `json:"teams"`
	Note  string       `json:"note"`
}

type Pending struct {
	Instructions int `json:"instructions"`
	Conflicts    int `json:"conflicts"`
	Reviews      int `json:"reviews"`
}

// Envelope is the server's common response shape. Fields the CLI never reads
// are kept raw so strict decoding still accepts them.
type Envelope struct {
	OK               bool            `json:"ok"`
	Key              string          `json:"key,omitempty"`
	ProjectKey       string          `json:"project_key,omitempty"`
	ParentSessionKey string          `json:"parent_session_key,omitempty"`
	Sequence         int64           `json:"sequence"`
	Revision         int64           `json:"revision"`
	Pending          Pending         `json:"pending"`
	Note             string          `json:"note,omitempty"`
	Conflicts        json.RawMessage `json:"conflicts,omitempty"`
	Review           json.RawMessage `json:"review,omitempty"`
}

type StateTeam struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

type StateSession struct {
	Key        string `json:"key"`
	Member     string `json:"member"`
	Agent      string `json:"agent"`
	Project    string `json:"project"`
	Branch     string `json:"branch,omitempty"`
	Goal       string `json:"goal,omitempty"`
	StatusLine string `json:"status_line,omitempty"`
	Status     string `json:"status"`
	LastSeen   string `json:"last_seen,omitempty"`
	Mine       bool   `json:"mine,omitempty"`
}

type StateProject struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	RepoURL        string `json:"repo_url,omitempty"`
	DefaultBranch  string `json:"default_branch,omitempty"`
	LiveSessions   int    `json:"live_sessions"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
}

type StateMemberAgent struct {
	Key           string `json:"key"`
	Label         string `json:"label"`
	ClientKind    string `json:"client_kind,omitempty"`
	Status        string `json:"status"`
	LastSessionAt string `json:"last_session_at,omitempty"`
}

type StateMember struct {
	Key         string             `json:"key"`
	DisplayName string             `json:"display_name"`
	Role        string             `json:"role"`
	JoinedAt    string             `json:"joined_at"`
	LastSeenAt  string             `json:"last_seen_at,omitempty"`
	Mine        bool               `json:"mine"`
	Agents      []StateMemberAgent `json:"agents"`
}

type TeamState struct {
	Envelope
	Scope      string          `json:"scope"`
	Team       *StateTeam      `json:"team,omitempty"`
	Sessions   []StateSession  `json:"sessions,omitempty"`
	Events     json.RawMessage `json:"events,omitempty"`
	Projects   []StateProject  `json:"projects,omitempty"`
	Members    []StateMember   `json:"members,omitempty"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
	Counts     json.RawMessage `json:"counts,omitempty"`
}

// CreateTeam is create_team's answer. Token is decoded into a Secret and
// never printed: the CLI treats its presence as a bug (§1.4.1).
type CreateTeam struct {
	Envelope
	Token        secret.Secret `json:"token,omitempty"`
	TokenScope   string        `json:"token_scope"`
	TokenKept    bool          `json:"token_kept"`
	ClientKey    string        `json:"client_key"`
	AccountKey   string        `json:"account_key"`
	TeamSlug     string        `json:"team_slug"`
	MemberKey    string        `json:"member_key"`
	AgentKey     string        `json:"agent_key"`
	TokenNote    string        `json:"token_note"`
	Rejoined     bool          `json:"rejoined,omitempty"`
	AuthHeader   secret.Secret `json:"auth_header,omitempty"`
	TeamName     string        `json:"team_name"`
	JoinCode     string        `json:"join_code"`
	JoinCodeNote string        `json:"join_code_note"`
	Plan         string        `json:"plan"`
	Visibility   string        `json:"visibility"`
	Created      bool          `json:"created"`
}

type CreateInvite struct {
	OK        bool      `json:"ok"`
	TeamSlug  string    `json:"team_slug"`
	InviteID  string    `json:"invite_id"`
	Label     string    `json:"label"`
	Code      string    `json:"code,omitempty"`
	MaxUses   int64     `json:"max_uses"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	Created   bool      `json:"created"`
	ShareNote string    `json:"share_note"`
}

type InviteInfo struct {
	InviteID     string     `json:"invite_id"`
	Label        string     `json:"label"`
	CreatedBy    string     `json:"created_by"`
	CreatedByYou bool       `json:"created_by_you"`
	Uses         int64      `json:"uses"`
	MaxUses      *int64     `json:"max_uses"`
	ExpiresAt    *time.Time `json:"expires_at"`
	State        string     `json:"state"`
	LastUsedAt   *time.Time `json:"last_used_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

type ListInvites struct {
	OK       bool         `json:"ok"`
	TeamSlug string       `json:"team_slug"`
	Scope    string       `json:"scope"`
	Invites  []InviteInfo `json:"invites"`
	More     bool         `json:"more,omitempty"`
	Note     string       `json:"note"`
}

type RevokeInvite struct {
	OK             bool      `json:"ok"`
	TeamSlug       string    `json:"team_slug"`
	InviteID       string    `json:"invite_id"`
	Label          string    `json:"label"`
	State          string    `json:"state"`
	AlreadyRevoked bool      `json:"already_revoked"`
	Uses           int64     `json:"uses"`
	MaxUses        *int64    `json:"max_uses"`
	RevokedAt      time.Time `json:"revoked_at"`
	Note           string    `json:"note"`
}

type OpenBoardTeam struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// OpenBoard: LoginURL carries a single-use sign-in secret in its fragment.
type OpenBoard struct {
	OK             bool           `json:"ok"`
	Team           *OpenBoardTeam `json:"team,omitempty"`
	BoardURL       string         `json:"board_url"`
	LoginURL       string         `json:"login_url"`
	LoginExpiresAt time.Time      `json:"login_expires_at"`
	SingleUse      bool           `json:"single_use"`
	Note           string         `json:"note"`
}
