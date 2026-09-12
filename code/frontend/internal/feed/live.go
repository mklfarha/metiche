package feed

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// Live reads one team from a real metiche backend: the snapshot first, then
// the SSE stream from exactly where the snapshot ended.
//
// # The two halves, and why there are two
//
// The board cannot be rebuilt from the event stream alone, and that is by
// design on the backend's side rather than an oversight. A frame carries the
// sequence, the kind, whether it was structural, the short key of the subject
// and a one-line summary — enough to say WHAT changed, deliberately not enough
// to say what the thing now is. So:
//
//	GET /v1/teams/{slug}            the state, coherent at one sequence
//	GET /v1/teams/{slug}/stream     what has happened since
//
// Snapshot is the first, Stream is the second, and the cursor the first
// returns is the argument the second is opened with. Everything at or below it
// is already drawn; everything above it arrives on the stream. No gap, and no
// event applied twice.
//
// # Reconnection
//
// Exact, not best-effort. The cursor advances only on frames actually handed
// downstream, and every reconnect re-issues ?after=<cursor>. The backend's
// contract is that everything strictly greater than after is delivered once
// each in order, so a dropped connection loses nothing and repeats nothing.
// (It also honours Last-Event-ID, which is what a browser's own EventSource
// sends; this client always says ?after= because it knows its cursor exactly
// and does not want to depend on which of the two a proxy preserved.)
//
// # Auth
//
// The read API is not public — a team's board is not public just because the
// repository is. Token is a bearer token supplied by the operator through the
// environment; it is never a command-line argument (that lands in shell
// history and in `ps`), never read from a file in the repo, and never logged.
type Live struct {
	// BaseURL is the backend root, with or without the /v1 suffix.
	BaseURL string
	Slug    string

	// Token is the bearer token for the read API. It comes from the
	// environment — see cmd/metiche-web. Empty means send no Authorization
	// header at all, which is only useful against a backend that has not
	// turned auth on yet.
	Token string

	Client *http.Client
	// Backoff is the pause before reconnecting. It does not escalate: the
	// backend is on the same cluster and a stream closed by an idle proxy is
	// the common case, not an outage.
	Backoff time.Duration

	// TimelineBackfill asks the stream to start this many sequences before the
	// snapshot's cursor, so a freshly loaded board has recent history in its
	// timeline instead of an empty rail.
	//
	// It is safe only because live entity state comes from the snapshot: the
	// backfilled events are appended to the log and never folded back over
	// entities they already produced. Zero — the zero value — asks for no
	// history at all and resumes exactly at the snapshot's cursor; the
	// operator-facing default lives on the -backfill flag rather than here, so
	// that a Live built in code does precisely what its fields say.
	TimelineBackfill int64

	// RequestTimeout bounds the snapshot reads. It is NOT applied to the
	// stream, which is meant to stay open.
	RequestTimeout time.Duration

	Logger *slog.Logger
}

// Name implements Feed. It must never grow the token: it is logged, and it is
// rendered on /healthz.
func (l *Live) Name() string { return "live:" + l.BaseURL }

func (l *Live) root() string {
	trimmed := strings.TrimRight(l.BaseURL, "/")
	if strings.HasSuffix(trimmed, "/v1") {
		return trimmed
	}
	return trimmed + "/v1"
}

func (l *Live) client() *http.Client {
	if l.Client != nil {
		return l.Client
	}
	// No overall timeout on the shared client: the stream request is meant to
	// stay open, and a Client timeout would cut it off mid-flight. The
	// snapshot reads get their own per-request deadline instead.
	return http.DefaultClient
}

func (l *Live) log() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// request builds an authenticated GET.
func (l *Live) request(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if l.Token != "" {
		req.Header.Set("Authorization", "Bearer "+l.Token)
	}
	return req, nil
}

// getJSON reads one board endpoint into v.
//
// The error never carries the response body: a 500 from the API is a problem
// document, and a badly behaved one could echo driver text, which can carry a
// DSN, which carries a password. Status and path are enough to debug with.
func (l *Live) getJSON(ctx context.Context, url string, v any) error {
	timeout := l.RequestTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := l.request(ctx, url)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := l.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", req.URL.Path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// ---------------------------------------------------------------- snapshot

// Snapshot implements Snapshotter: one coherent read of the whole team.
//
// Three calls, and the ORDER of them is load-bearing. The team snapshot goes
// first because its sequence becomes the resume cursor, and the contracts and
// decisions are read after it. Read the other way round, a contract published
// between the two reads would have a sequence at or below the cursor — already
// streamed past, not in the state, and therefore missing until something else
// happened to trigger a refresh. Read in this order the extra reads can only
// be AHEAD of the cursor, and being ahead is harmless: the stream frame that
// reports the same change arrives later and merely refreshes state that
// already reflects it.
func (l *Live) Snapshot(ctx context.Context) (model.TeamState, error) {
	base := l.root() + "/teams/" + url.PathEscape(l.Slug)

	var snap snapshotWire
	if err := l.getJSON(ctx, base, &snap); err != nil {
		return model.TeamState{}, err
	}
	var contracts contractsWire
	if err := l.getJSON(ctx, base+"/contracts", &contracts); err != nil {
		return model.TeamState{}, err
	}
	var decisions decisionsWire
	if err := l.getJSON(ctx, base+"/decisions", &decisions); err != nil {
		return model.TeamState{}, err
	}

	ts := snap.teamState(l.Slug)
	ts.Contracts = contracts.contracts()
	ts.Decisions = decisions.decisions()

	if l.TimelineBackfill > 0 {
		ts.ResumeFrom = max64(0, ts.Team.Sequence-l.TimelineBackfill)
	}
	return ts, nil
}

// ---------------------------------------------------------------- stream

// Stream implements Feed.
func (l *Live) Stream(ctx context.Context, after int64) (<-chan model.Event, error) {
	backoff := l.Backoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}

	out := make(chan model.Event, 64)
	go func() {
		defer close(out)
		cursor := after
		for ctx.Err() == nil {
			n, err := l.pump(ctx, cursor, out)
			cursor = max64(cursor, n)
			if err != nil && ctx.Err() == nil {
				l.log().Warn("metiche stream dropped", "slug", l.Slug, "cursor", cursor, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}()
	return out, nil
}

// pump holds one connection open and returns the highest sequence delivered.
func (l *Live) pump(ctx context.Context, after int64, out chan<- model.Event) (int64, error) {
	target := fmt.Sprintf("%s/teams/%s/stream?after=%d", l.root(), url.PathEscape(l.Slug), after)
	req, err := l.request(ctx, target)
	if err != nil {
		return after, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := l.client().Do(req)
	if err != nil {
		return after, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return after, fmt.Errorf("GET %s: %s", req.URL.Path, resp.Status)
	}

	cursor := after
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		data    strings.Builder
		eventID int64
		haveID  bool
	)
	// flush ends one SSE message.
	//
	// A frame that will not parse advances the cursor to its id: rather than
	// leaving it behind. The backend guarantees the id IS the sequence, so
	// this is the difference between skipping one unusable frame and
	// re-requesting it on every reconnect forever.
	flush := func() error {
		defer func() {
			data.Reset()
			eventID, haveID = 0, false
		}()
		if data.Len() == 0 {
			return nil
		}
		var f frameWire
		if err := json.Unmarshal([]byte(data.String()), &f); err != nil {
			l.log().Warn("skipping an unparseable stream frame", "slug", l.Slug, "id", eventID)
			if haveID {
				cursor = max64(cursor, eventID)
			}
			return nil
		}
		if f.Sequence == 0 && haveID {
			f.Sequence = eventID
		}
		if f.Sequence <= cursor {
			// Already delivered. The backend does not resend, but a proxy
			// replaying a buffer or an overlapping reconnect can, and the
			// cheapest place to make that harmless is here.
			return nil
		}
		cursor = f.Sequence
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- f.event():
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return cursor, err
			}
		case strings.HasPrefix(line, ":"):
			// Comment / keepalive.
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "id:"):
			if n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64); err == nil {
				eventID, haveID = n, true
			}
		case strings.HasPrefix(line, "event:"):
			// The event name is the kind, which is also in the JSON body.
			// Reading it from the body keeps one parse path.
		}
	}
	if err := scanner.Err(); err != nil {
		return cursor, err
	}
	return cursor, flush()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
