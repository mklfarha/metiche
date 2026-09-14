package mcp

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// get_team_state scope=members (docs/CLI.md §4.9)
// ─────────────────────────────────────────────
//
// Who is on the team and which agents they run THERE, for `metiche teams
// show`. A scope rather than a tool for §4.2's reason. Members see members, as
// the board already shows them.
//
// What it guarantees:
//
//   - LIVE MEMBERS ONLY (revoked_at IS NULL, status active); owners first,
//     then by when they joined.
//   - A TEAMMATE'S AGENTS ONLY WHERE THEY WORKED HERE. Agents are account-wide,
//     so listing every one would show teammates the labels of clients a person
//     uses on OTHER teams. A member's agents are those with at least one
//     session on this team; your own row (mine: true) lists all your active
//     agents as well.
//   - NOTHING DERIVED FROM A TOKEN: no client_key, no account key.

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

func (h *Handler) listMembers(ctx context.Context, db *sql.DB, who Resolved, cursor string, limit int) ([]StateMember, string, error) {
	offset, err := decodeOffsetCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	rows, err := db.QueryContext(ctx,
		"SELECT `id`, `key`, `display_name`, `role`, `created_at`, `last_seen_at` FROM `member` "+
			"WHERE `team_uuid` = ? AND `revoked_at` IS NULL AND `status` = ? LIMIT ?",
		who.Team.ID.String(), enums.RECORD_STATUS_ACTIVE, stateScopeScanMax)
	if err != nil {
		return nil, "", retryable(err, "listing the team's members")
	}
	type memberRow struct {
		id     string
		m      StateMember
		owner  bool
		joined time.Time
	}
	var members []memberRow
	for rows.Next() {
		var (
			r        memberRow
			role     int64
			lastSeen sql.NullTime
		)
		if err := rows.Scan(&r.id, &r.m.Key, &r.m.DisplayName, &role, &r.joined, &lastSeen); err != nil {
			_ = rows.Close()
			return nil, "", err
		}
		r.owner = enums.MemberRole(role) == enums.MEMBER_ROLE_OWNER
		r.m.Role = enums.MemberRole(role).String()
		r.m.JoinedAt = r.joined.UTC().Format(time.RFC3339)
		if lastSeen.Valid {
			r.m.LastSeenAt = lastSeen.Time.UTC().Format(time.RFC3339)
		}
		r.m.Mine = r.id == who.Member.ID.String()
		r.m.Agents = []StateMemberAgent{}
		members = append(members, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, "", retryable(err, "listing the team's members")
	}
	_ = rows.Close()

	// Agents with at least one session on THIS team, by the member whose
	// sessions they were.
	type agentRow struct {
		id   string
		a    StateMemberAgent
		last time.Time
	}
	byMember := map[string][]agentRow{}
	arows, err := db.QueryContext(ctx,
		"SELECT s.`member_uuid`, a.`id`, a.`key`, a.`label`, COALESCE(a.`client_kind`, ''), a.`status`, "+
			"MAX(s.`started_at`), MAX(s.`created_at`) "+
			"FROM `session` s JOIN `agent` a ON a.`id` = s.`agent_uuid` WHERE s.`team_uuid` = ? "+
			"GROUP BY s.`member_uuid`, a.`id`, a.`key`, a.`label`, a.`client_kind`, a.`status` LIMIT ?",
		who.Team.ID.String(), stateScopeScanMax)
	if err != nil {
		return nil, "", retryable(err, "listing the team's agents")
	}
	for arows.Next() {
		var (
			memberID      string
			r             agentRow
			status        int64
			started, made sql.NullTime
		)
		if err := arows.Scan(&memberID, &r.id, &r.a.Key, &r.a.Label, &r.a.ClientKind, &status, &started, &made); err != nil {
			_ = arows.Close()
			return nil, "", err
		}
		r.a.Status = enums.AgentStatus(status).String()
		for _, t := range []sql.NullTime{started, made} {
			if t.Valid && t.Time.After(r.last) {
				r.last = t.Time.UTC()
			}
		}
		if !r.last.IsZero() {
			r.a.LastSessionAt = r.last.Format(time.RFC3339)
		}
		byMember[memberID] = append(byMember[memberID], r)
	}
	if err := arows.Err(); err != nil {
		_ = arows.Close()
		return nil, "", retryable(err, "listing the team's agents")
	}
	_ = arows.Close()

	// Your own row also lists every active agent of yours.
	own, err := db.QueryContext(ctx,
		"SELECT `id`, `key`, `label`, COALESCE(`client_kind`, ''), `status` FROM `agent` "+
			"WHERE `account_uuid` = ? AND `status` = ? LIMIT ?",
		who.Account.ID.String(), enums.AGENT_STATUS_ACTIVE, stateScopeScanMax)
	if err != nil {
		return nil, "", retryable(err, "listing your agents")
	}
	mineID := who.Member.ID.String()
	seen := map[string]bool{}
	for _, r := range byMember[mineID] {
		seen[r.id] = true
	}
	for own.Next() {
		var (
			r      agentRow
			status int64
		)
		if err := own.Scan(&r.id, &r.a.Key, &r.a.Label, &r.a.ClientKind, &status); err != nil {
			_ = own.Close()
			return nil, "", err
		}
		if seen[r.id] {
			continue
		}
		r.a.Status = enums.AgentStatus(status).String()
		byMember[mineID] = append(byMember[mineID], r)
	}
	if err := own.Err(); err != nil {
		_ = own.Close()
		return nil, "", retryable(err, "listing your agents")
	}
	_ = own.Close()

	for i := range members {
		agents := byMember[members[i].id]
		sort.SliceStable(agents, func(a, b int) bool {
			if !agents[a].last.Equal(agents[b].last) {
				return agents[a].last.After(agents[b].last)
			}
			return agents[a].a.Key < agents[b].a.Key
		})
		for _, a := range agents {
			members[i].m.Agents = append(members[i].m.Agents, a.a)
		}
	}

	sort.SliceStable(members, func(i, j int) bool {
		if members[i].owner != members[j].owner {
			return members[i].owner
		}
		if !members[i].joined.Equal(members[j].joined) {
			return members[i].joined.Before(members[j].joined)
		}
		return members[i].m.Key < members[j].m.Key
	})

	out := []StateMember{}
	for i := offset; i < len(members) && len(out) < limit; i++ {
		out = append(out, members[i].m)
	}
	return out, nextOffsetCursor(offset, len(out), len(members)), nil
}
