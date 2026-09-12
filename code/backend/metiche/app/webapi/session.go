package webapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

// maxSessionEvents bounds one page of a session's history. A long-running
// session can produce thousands of events; the page reads them with
// ?after=<last sequence>, the same cursor the stream uses.
const maxSessionEvents = 500

type sessionResponse struct {
	cursors
	Team    teamRef     `json:"team"`
	Session sessionWire `json:"session"`
	Events  []eventWire `json:"events"`

	// NextAfter is the cursor for the next page, present only when there is
	// one. It is a sequence, the same unit as everything else in this API, so
	// a caller never has to learn a second pagination vocabulary.
	NextAfter *int64 `json:"next_after,omitempty"`
}

var errSessionNotFound = errors.New("no such session")

// handleSession serves GET /v1/teams/{slug}/sessions/{key}?after=N&limit=M.
//
// A session's history IS the event log filtered by session — the model has no
// separate record of steps, on purpose — so this endpoint is that filter, plus
// the session row itself so the page has a heading.
//
// It reads inside the same read-only REPEATABLE READ transaction as everything
// else here, which matters more than it looks: the session row and its events
// are two queries, and without one snapshot a page could show a session that
// has ended above an event log that does not contain the ending.
func (a *API) handleSession(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	key := chi.URLParam(r, "key")
	after := int64Query(r, "after")
	limit := intQuery(r, "limit", 200, 1, maxSessionEvents)

	var out sessionResponse
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		sessionUUID, s, err := loadSession(r.Context(), tx, team.UUID, key)
		if err != nil {
			return err
		}
		out.Session = s

		if err := attachIntents(r.Context(), tx, team.UUID,
			map[string]*sessionWire{sessionUUID: &out.Session}); err != nil {
			return err
		}
		if _, err := attachClaims(r.Context(), tx, team.UUID,
			map[string]*sessionWire{sessionUUID: &out.Session}); err != nil {
			return err
		}

		events, err := loadSessionEvents(r.Context(), tx, sessionUUID, after, limit)
		if err != nil {
			return err
		}
		out.Events = events
		if len(events) == limit {
			next := events[len(events)-1].Sequence
			out.NextAfter = &next
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			writeProblem(w, http.StatusNotFound, "not found", "no such session on this team")
			return
		}
		a.fail(w, r, err, "read the session")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

const sessionQuery = "SELECT s.`id`, s.`key`, s.`branch`, s.`goal`, s.`status`, s.`status_line`, " +
	"s.`started_at`, s.`last_heartbeat_at`, s.`ended_at`, s.`outcome`, " +
	"p.`key`, m.`key`, m.`display_name`, a.`label`, a.`client_kind`, ci.`key` " +
	"FROM `session` s " +
	"JOIN `project` p ON p.`id` = s.`project_uuid` " +
	"JOIN `member` m ON m.`id` = s.`member_uuid` " +
	"JOIN `agent` a ON a.`id` = s.`agent_uuid` " +
	"LEFT JOIN `intent` ci ON ci.`id` = s.`current_intent_uuid` " +
	"WHERE s.`team_uuid` = ? AND s.`key` = ? LIMIT 1"

// loadSession reads one session by its short key, served by the
// (team_uuid, key) unique index.
func loadSession(ctx context.Context, tx *sql.Tx, teamUUID, key string) (string, sessionWire, error) {
	var (
		id                                                      string
		s                                                       sessionWire
		branch, goal, statusLine                                sql.NullString
		started, heartbeat, ended                               sql.NullTime
		status, outcome                                         sql.NullInt64
		projectKey, memberKey, memberName, agentLabel, clientKd sql.NullString
		currentIntent                                           sql.NullString
	)
	err := tx.QueryRowContext(ctx, sessionQuery, teamUUID, key).Scan(
		&id, &s.Key, &branch, &goal, &status, &statusLine,
		&started, &heartbeat, &ended, &outcome,
		&projectKey, &memberKey, &memberName, &agentLabel, &clientKd, &currentIntent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sessionWire{}, errSessionNotFound
	}
	if err != nil {
		return "", sessionWire{}, err
	}

	s.Branch, s.Goal, s.StatusLine = branch.String, goal.String, statusLine.String
	s.Status = enums.SessionStatus(status.Int64).String()
	if outcome.Valid && enums.SessionOutcome(outcome.Int64) != enums.SESSION_OUTCOME_INVALID {
		s.Outcome = enums.SessionOutcome(outcome.Int64).String()
	}
	s.StartedAt, s.LastHeartbeatAt, s.EndedAt = rfc3339(started), rfc3339(heartbeat), rfc3339(ended)
	s.ProjectKey, s.MemberKey, s.MemberName = projectKey.String, memberKey.String, memberName.String
	s.AgentLabel, s.ClientKind = agentLabel.String, clientKd.String
	s.CurrentIntentKey = currentIntent.String
	s.Intents = []intentWire{}
	s.Claims = []claimWire{}
	return id, s, nil
}

const sessionEventsQuery = "SELECT e.`sequence`, e.`kind`, e.`structural`, e.`subject_kind`, " +
	"e.`subject_key`, e.`summary`, e.`occurred_at` " +
	"FROM `team_event` e WHERE e.`session_uuid` = ? AND e.`sequence` > ? " +
	"ORDER BY e.`sequence` LIMIT ?"

// loadSessionEvents reads one page of a session's history, served directly by
// the (session_uuid, sequence) index.
func loadSessionEvents(ctx context.Context, tx *sql.Tx, sessionUUID string, after int64, limit int) ([]eventWire, error) {
	rows, err := tx.QueryContext(ctx, sessionEventsQuery, sessionUUID, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]eventWire, 0, 32)
	for rows.Next() {
		var (
			e                      eventWire
			kind                   int64
			subjectKind            sql.NullInt64
			subjectKey, summary    sql.NullString
			occurred               sql.NullTime
			structuralFromDatabase bool
		)
		if err := rows.Scan(&e.Sequence, &kind, &structuralFromDatabase, &subjectKind,
			&subjectKey, &summary, &occurred); err != nil {
			return nil, err
		}
		e.Kind = enums.EventKind(kind).String()
		e.Structural = structuralFromDatabase
		if subjectKind.Valid && enums.SubjectKind(subjectKind.Int64) != enums.SUBJECT_KIND_INVALID {
			e.SubjectKind = enums.SubjectKind(subjectKind.Int64).String()
		}
		e.SubjectKey, e.Summary = subjectKey.String, summary.String
		e.OccurredAt = rfc3339(occurred)
		out = append(out, e)
	}
	return out, rows.Err()
}
