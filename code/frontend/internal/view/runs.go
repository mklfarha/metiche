package view

import (
	"fmt"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
)

// RunRow is one line of the Runs list. A live session from the hub and a run
// from the backend's history are both reduced to it, so the two halves of the
// page draw the same row.
type RunRow struct {
	Key      string
	Agent    string // "Member · agent-label"
	Project  string
	Headline string // the goal, or the status line when there is no goal
	Status   string
	Outcome  string
	Live     bool

	StartedAt time.Time
	EndedAt   time.Time

	Intents   int
	Paths     int
	Conflicts int
}

// RunsParams is the Runs page of a board read from the backend.
type RunsParams struct {
	Slug string
	Now  time.Time

	Live    []RunRow // from the hub
	History []RunRow // one page from the backend, without anything in Live

	// OlderURL loads the next, older page; "" when there is none.
	OlderURL string
	// Older is true when the page starts from a cursor rather than the newest
	// run (a "Load older" followed without JavaScript).
	Older bool
	// HistoryUnavailable is true when the backend could not be read; History
	// then holds what the hub has.
	HistoryUnavailable bool
}

// RunDetailParams is one run told from the backend.
type RunDetailParams struct {
	Slug   string
	Detail model.RunDetail
}

func runWhen(now time.Time, r RunRow) string {
	switch {
	case r.Live || (r.EndedAt.IsZero() && !r.StartedAt.IsZero()):
		return "started " + state.Ago(now, r.StartedAt) + " ago"
	case !r.EndedAt.IsZero():
		s := "ended " + state.Ago(now, r.EndedAt) + " ago"
		if !r.StartedAt.IsZero() && r.EndedAt.After(r.StartedAt) {
			s += " · ran " + runDuration(r.EndedAt.Sub(r.StartedAt))
		}
		return s
	}
	return "not started"
}

func runDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func runCounts(r RunRow) string {
	return countOf(r.Intents, "intent", "intents") + " · " +
		countOf(r.Paths, "path", "paths") + " · " +
		countOf(r.Conflicts, "conflict", "conflicts")
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// stamp is an absolute time, for history, where "3d ago" loses the when.
func runStamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func outcomeBadge(outcome string) string {
	if outcome == "succeeded" {
		return "good"
	}
	return "plain"
}

func runPip(status string) string { return pipClass(&model.Session{Status: status}) }
