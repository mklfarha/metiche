package mcp

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// get_team_state: the team block and scope=projects (docs/CLI.md §4.2)
// ─────────────────────────────────────────────
//
// Nothing listed projects: list_teams deliberately carries five fields, and
// every one is about choosing a team. Projects are team state, so they are a
// scope of get_team_state. The team block rides on every scope so a person's
// tool can tell a board a browser can show (public) from one that 404s
// (private) without a second call; RequireTeam has already loaded the row.

// stateScopeScanMax bounds the rows a projects or members page is cut from. A
// team's projects are its repositories and its members are capped by the
// plan, so this is a runaway guard, not a product limit.
const stateScopeScanMax = 500

// StateTeam is the team block on every get_team_state response.
type StateTeam struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// StateProject is one active project.
type StateProject struct {
	Key string `json:"key"`
	// Name is the project's display name.
	Name string `json:"name"`
	// RepoURL is the stored canonical form (https://host/path, credentials
	// never stored), which normalizes to the repository identity start_session
	// matches by. Absent when the project has no repository recorded.
	RepoURL       string `json:"repo_url,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	// LiveSessions counts sessions with status live.
	LiveSessions int `json:"live_sessions"`
	// LastActivityAt is the latest started_at or last_heartbeat_at over the
	// project's sessions; absent for a project nobody has worked in.
	LastActivityAt string `json:"last_activity_at,omitempty"`
}

func teamBlock(r Resolved) *StateTeam {
	return &StateTeam{Slug: r.Team.Slug, Name: r.Team.Name, Visibility: r.Team.Visibility.String()}
}

// listProjects returns one page of the team's ACTIVE projects, most recently
// active first, then by key.
func (h *Handler) listProjects(ctx context.Context, db *sql.DB, teamUUID uuid.UUID, cursor string, limit int) ([]StateProject, string, error) {
	offset, err := decodeOffsetCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := db.QueryContext(ctx,
		"SELECT p.`key`, p.`name`, COALESCE(p.`repo_url`, ''), COALESCE(p.`default_branch`, ''), "+
			"(SELECT COUNT(*) FROM `session` s WHERE s.`project_uuid` = p.`id` AND s.`status` = ?), "+
			"(SELECT MAX(s.`started_at`) FROM `session` s WHERE s.`project_uuid` = p.`id`), "+
			"(SELECT MAX(s.`last_heartbeat_at`) FROM `session` s WHERE s.`project_uuid` = p.`id`) "+
			"FROM `project` p WHERE p.`team_uuid` = ? AND p.`status` = ? LIMIT ?",
		enums.SESSION_STATUS_LIVE, teamUUID.String(), enums.RECORD_STATUS_ACTIVE, stateScopeScanMax)
	if err != nil {
		return nil, "", retryable(err, "listing the team's projects")
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		p    StateProject
		last time.Time
	}
	var all []row
	for rows.Next() {
		var (
			r                  row
			started, heartbeat sql.NullTime
		)
		if err := rows.Scan(&r.p.Key, &r.p.Name, &r.p.RepoURL, &r.p.DefaultBranch, &r.p.LiveSessions, &started, &heartbeat); err != nil {
			return nil, "", err
		}
		for _, t := range []sql.NullTime{started, heartbeat} {
			if t.Valid && t.Time.After(r.last) {
				r.last = t.Time.UTC()
			}
		}
		if !r.last.IsZero() {
			r.p.LastActivityAt = r.last.Format(time.RFC3339)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", retryable(err, "listing the team's projects")
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].last.Equal(all[j].last) {
			return all[i].last.After(all[j].last)
		}
		return all[i].p.Key < all[j].p.Key
	})

	out := []StateProject{}
	for i := offset; i < len(all) && len(out) < limit; i++ {
		out = append(out, all[i].p)
	}
	return out, nextOffsetCursor(offset, len(out), len(all)), nil
}

// The projects and members scopes page with an opaque offset: both lists are
// read whole (bounded by stateScopeScanMax) and sorted in Go, because their
// orderings — latest activity, owners first — are not columns a keyset could
// seek on.
const offsetCursorPrefix = "offset:"

var errBadCursor = errors.New("cursor is not a cursor this server issued; omit it to start from the beginning")

func decodeOffsetCursor(cursor string) (int, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || !strings.HasPrefix(string(raw), offsetCursorPrefix) {
		return 0, errBadCursor
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), offsetCursorPrefix))
	if err != nil || n < 0 {
		return 0, errBadCursor
	}
	return n, nil
}

func nextOffsetCursor(offset, page, total int) string {
	if offset+page >= total {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(offsetCursorPrefix + strconv.Itoa(offset+page)))
}
