package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// RunHistory reads a team's run history from the backend.
//
// A Live implements it, and reads with exactly the principal it reads the
// board with: nothing for a public team, the one viewer's browser session for
// a private board. A Fixture does not implement it, so a demo board has no way
// to ask the backend anything.
type RunHistory interface {
	// Runs reads one page of sessions, newest first. cursor is "" for the
	// newest page, or a RunPage.NextCursor.
	Runs(ctx context.Context, cursor string, limit int) (model.RunPage, error)
	// Run reads one session's whole story.
	Run(ctx context.Context, key string) (model.RunDetail, error)
}

var _ RunHistory = (*Live)(nil)

const (
	// runEventPageSize is the backend's own maximum page of a run's events.
	runEventPageSize = 500
	// runEventPages bounds how many pages one run detail reads.
	runEventPages = 4
	// maxHistoryBody bounds one history answer.
	maxHistoryBody = 8 << 20
)

// ---------------------------------------------------------------- wire

type countsJSON struct {
	Intents      int `json:"intents"`
	ClaimedPaths int `json:"claimed_paths"`
	Conflicts    int `json:"conflicts"`
}

// historySessionJSON is sessionJSON plus the two fields only history carries.
// Its intents and claims, when present, are what the session holds right
// now, and are not read here.
type historySessionJSON struct {
	sessionJSON
	OutcomeNote string     `json:"outcome_note"`
	Counts      countsJSON `json:"counts"`
}

type sessionsPageWire struct {
	Sessions   []historySessionJSON `json:"sessions"`
	NextCursor string               `json:"next_cursor"`
}

type runIntentJSON struct {
	intentJSON
	StartedAt *string `json:"started_at"`
	EndedAt   *string `json:"ended_at"`
}

type runClaimJSON struct {
	claimJSON
	Status     string  `json:"status"`
	ClaimedAt  *string `json:"claimed_at"`
	ReleasedAt *string `json:"released_at"`
}

type runEventJSON struct {
	Sequence   int64   `json:"sequence"`
	Kind       string  `json:"kind"`
	Structural bool    `json:"structural"`
	SubjectKey string  `json:"subject_key"`
	Summary    string  `json:"summary"`
	OccurredAt *string `json:"occurred_at"`
}

type runDetailWire struct {
	Session   historySessionJSON `json:"session"`
	Events    []runEventJSON     `json:"events"`
	NextAfter *int64             `json:"next_after"`
	History   struct {
		Intents   []runIntentJSON `json:"intents"`
		Claims    []runClaimJSON  `json:"claims"`
		Conflicts []conflictJSON  `json:"conflicts"`
	} `json:"history"`
}

func (s historySessionJSON) summary() model.RunSummary {
	return model.RunSummary{
		Key: s.Key, ProjectKey: s.ProjectKey, MemberKey: s.MemberKey, MemberName: s.MemberName,
		AgentLabel: s.AgentLabel, ClientKind: s.ClientKind,
		Branch: s.Branch, Goal: s.Goal, Status: s.Status, StatusLine: s.StatusLine,
		StartedAt: parseTime(s.StartedAt), LastHeartbeatAt: parseTime(s.LastHeartbeatAt), EndedAt: parseTime(s.EndedAt),
		Outcome: s.Outcome, OutcomeNote: s.OutcomeNote,
		Intents: s.Counts.Intents, ClaimedPaths: s.Counts.ClaimedPaths, Conflicts: s.Counts.Conflicts,
	}
}

// ---------------------------------------------------------------- reads

// Runs implements RunHistory: GET /v1/teams/{slug}/sessions.
func (l *Live) Runs(ctx context.Context, cursor string, limit int) (model.RunPage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	target := l.root() + "/teams/" + url.PathEscape(l.Slug) + "/sessions"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var wire sessionsPageWire
	if err := l.readHistory(ctx, target, &wire); err != nil {
		return model.RunPage{}, err
	}
	out := model.RunPage{NextCursor: wire.NextCursor, Runs: make([]model.RunSummary, 0, len(wire.Sessions))}
	for _, s := range wire.Sessions {
		out.Runs = append(out.Runs, s.summary())
	}
	return out, nil
}

// Run implements RunHistory: GET /v1/teams/{slug}/sessions/{key}, reading the
// event pages until the log ends or runEventPages have been read.
func (l *Live) Run(ctx context.Context, key string) (model.RunDetail, error) {
	base := l.root() + "/teams/" + url.PathEscape(l.Slug) + "/sessions/" + url.PathEscape(key)
	var out model.RunDetail
	after := int64(0)
	for page := 0; page < runEventPages; page++ {
		var wire runDetailWire
		if err := l.readHistory(ctx, fmt.Sprintf("%s?limit=%d&after=%d", base, runEventPageSize, after), &wire); err != nil {
			return model.RunDetail{}, err
		}
		if page == 0 {
			out.Run = wire.Session.summary()
			for _, in := range wire.History.Intents {
				out.Intents = append(out.Intents, model.RunIntent{
					Key: in.Key, Summary: in.Summary, Kind: in.Kind, Status: in.Status,
					ExternalRef: in.ExternalRef, Revision: int(in.Revision),
					DeclaredAt: parseTime(in.DeclaredAt), StartedAt: parseTime(in.StartedAt), EndedAt: parseTime(in.EndedAt),
				})
			}
			for _, c := range wire.History.Claims {
				out.Claims = append(out.Claims, model.RunClaim{
					Key: c.Key, Mode: c.Mode, Status: c.Status, Paths: c.Paths,
					ClaimedAt: parseTime(c.ClaimedAt), ExpiresAt: parseTime(c.ExpiresAt), ReleasedAt: parseTime(c.ReleasedAt),
				})
			}
			memberBySession := map[string]string{out.Run.Key: out.Run.MemberKey}
			for _, c := range wire.History.Conflicts {
				out.Conflicts = append(out.Conflicts, c.conflict(memberBySession))
			}
		}
		for _, e := range wire.Events {
			out.Events = append(out.Events, model.Event{
				Sequence: e.Sequence, Kind: e.Kind, Summary: e.Summary, Structural: e.Structural,
				SessionKey: out.Run.Key, MemberKey: out.Run.MemberKey,
				AgentKey:   agentKey(out.Run.MemberKey, out.Run.AgentLabel),
				OccurredAt: parseTime(e.OccurredAt),
			})
		}
		if wire.NextAfter == nil {
			return out, nil
		}
		after = *wire.NextAfter
	}
	out.MoreEvents = true
	return out, nil
}

// readHistory is getJSON for the history reads, with one difference that
// matters: a 404 is returned as ErrNotFound WITHOUT the not-found report.
// A history read names a session as well as the team, and "no such session"
// must never take a board down; whether the team itself is still readable is
// the snapshot's and the stream's call. A 401 for a browser session is
// ErrSessionInvalid.
//
// No error carries the slug, the session key or the response body.
func (l *Live) readHistory(ctx context.Context, target string, v any) error {
	timeout := l.RequestTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := l.request(ctx, target)
	if err != nil {
		return fmt.Errorf("run history: bad request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.client().Do(req)
	if err != nil {
		return fmt.Errorf("run history: backend unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxHistoryBody)).Decode(v); err != nil {
			return fmt.Errorf("run history: malformed answer")
		}
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("run history: %w", ErrNotFound)
	case resp.StatusCode == http.StatusUnauthorized && l.BrowserSession != "":
		return ErrSessionInvalid
	}
	return fmt.Errorf("run history: backend answered %d", resp.StatusCode)
}
