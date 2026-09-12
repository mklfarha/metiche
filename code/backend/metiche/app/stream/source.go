package stream

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

// batchLimit caps how many frames one read returns.
//
// It bounds two things at once: the replay a client asks for with ?after=0 on a
// team with a long history, and the catch-up a tailer does after the process
// was wedged. Both loop until a short batch comes back, so the cap costs an
// extra round trip and never truncates the stream.
const batchLimit = 500

// Source produces frames for a team after a cursor, oldest first.
//
// It is an interface for two reasons. The obvious one is that the hub's
// ordering and deduplication are the subtle part of this package and they are
// worth testing without a database in the way. The less obvious one is that
// this is the only place the stream touches storage, so a future backed by
// something other than a poll over team_event replaces this and nothing else.
type Source interface {
	FramesAfter(ctx context.Context, teamUUID uuid.UUID, after int64, limit int) ([]Frame, error)
}

// TeamRef is the little that the stream needs to know about a team.
//
// Deliberately not the whole row: the team row also holds the join code, and a
// type that carries a secret is a type that eventually logs one.
type TeamRef struct {
	UUID          uuid.UUID
	Slug          string
	Sequence      int64
	BoardRevision int64
}

// TeamLookup resolves the {slug} in the URL to a team.
type TeamLookup func(ctx context.Context, slug string) (TeamRef, error)

// ErrTeamNotFound is returned by a TeamLookup for an unknown slug. It is
// deliberately blunt: the handler turns it into a 404 with no detail, because
// distinguishing "no such team" from "not yours" over a public endpoint is how
// a team roster leaks.
var ErrTeamNotFound = errors.New("no such team")

// dbSource reads frames from team_event.
type dbSource struct{ db *sql.DB }

// NewDBSource returns the Source that backs production: one indexed range scan
// over team_event per read, served directly by the (team_uuid, sequence)
// unique index.
func NewDBSource(db *sql.DB) Source { return &dbSource{db: db} }

const framesQuery = "SELECT e.`sequence`, e.`kind`, e.`structural`, e.`subject_kind`, e.`subject_key`, " +
	"e.`summary`, e.`payload`, e.`occurred_at`, t.`board_revision`, " +
	"p.`key`, s.`key`, m.`key`, m.`display_name`, a.`label` " +
	"FROM `team_event` e " +
	"JOIN `team` t ON t.`id` = e.`team_uuid` " +
	"LEFT JOIN `project` p ON p.`id` = e.`project_uuid` " +
	"LEFT JOIN `session` s ON s.`id` = e.`session_uuid` " +
	"LEFT JOIN `member` m ON m.`id` = e.`member_uuid` " +
	"LEFT JOIN `agent` a ON a.`id` = e.`agent_uuid` " +
	"WHERE e.`team_uuid` = ? AND e.`sequence` > ? " +
	"ORDER BY e.`sequence` LIMIT ?"

// FramesAfter implements Source.
//
// The four joins are all LEFT: a team-level event (a member joining, a decision
// recorded outside any session) legitimately has no session, agent or project,
// and an inner join would drop exactly those frames — leaving the client with a
// permanent hole in its sequence that it would keep trying to reconnect over.
func (s *dbSource) FramesAfter(ctx context.Context, teamUUID uuid.UUID, after int64, limit int) ([]Frame, error) {
	if limit <= 0 || limit > batchLimit {
		limit = batchLimit
	}
	rows, err := s.db.QueryContext(ctx, framesQuery, teamUUID.String(), after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]Frame, 0, 16)
	for rows.Next() {
		var (
			f           Frame
			kind        int64
			subjectKind sql.NullInt64
			subjectKey  sql.NullString
			summary     sql.NullString
			payload     []byte
			projectKey  sql.NullString
			sessionKey  sql.NullString
			memberKey   sql.NullString
			memberName  sql.NullString
			agentLabel  sql.NullString
		)
		if err := rows.Scan(&f.Sequence, &kind, &f.Structural, &subjectKind, &subjectKey,
			&summary, &payload, &f.OccurredAt, &f.BoardRevision,
			&projectKey, &sessionKey, &memberKey, &memberName, &agentLabel); err != nil {
			return nil, err
		}
		f.Kind = enums.EventKind(kind).String()
		if subjectKind.Valid {
			if sk := enums.SubjectKind(subjectKind.Int64); sk != enums.SUBJECT_KIND_INVALID {
				f.SubjectKind = sk.String()
			}
		}
		f.SubjectKey = subjectKey.String
		f.Summary = summary.String
		f.ProjectKey = projectKey.String
		f.SessionKey = sessionKey.String
		f.MemberKey = memberKey.String
		f.MemberName = memberName.String
		f.AgentLabel = agentLabel.String
		f.Payload = framePayloadFrom(payload)
		out = append(out, f)
	}
	return out, rows.Err()
}

// NewDBTeamLookup resolves a team by its slug.
//
// It also accepts a uuid, because this package's route sits at
// /v1/teams/{slug} and chi matches a root-level route ahead of the generated
// CRUD mount — so /v1/teams/<uuid> arrives here too. Accepting both means
// addressing a team by its id keeps working instead of 404ing.
//
// Two point lookups at worst, both on a unique index, and the columns are
// chosen by name: join_code is never read, so it can never be returned.
func NewDBTeamLookup(db *sql.DB) TeamLookup {
	return func(ctx context.Context, slug string) (TeamRef, error) {
		// col is never user input — it is one of the two literals below — so the
		// concatenation is a constant choice, not an injected value. The slug
		// itself is always a bound parameter.
		scan := func(col, val string) (TeamRef, error) {
			var id string
			var ref TeamRef
			err := db.QueryRowContext(ctx,
				"SELECT `id`, `slug`, `sequence`, `board_revision` FROM `team` WHERE `"+col+"` = ? LIMIT 1",
				val).Scan(&id, &ref.Slug, &ref.Sequence, &ref.BoardRevision)
			if err != nil {
				return TeamRef{}, err
			}
			ref.UUID, err = uuid.FromString(id)
			if err != nil {
				return TeamRef{}, err
			}
			return ref, nil
		}

		ref, err := scan("slug", slug)
		if err == nil {
			return ref, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return TeamRef{}, err
		}
		if _, perr := uuid.FromString(slug); perr != nil {
			return TeamRef{}, ErrTeamNotFound
		}
		ref, err = scan("id", slug)
		if errors.Is(err, sql.ErrNoRows) {
			return TeamRef{}, ErrTeamNotFound
		}
		if err != nil {
			return TeamRef{}, err
		}
		return ref, nil
	}
}
