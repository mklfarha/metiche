package feed

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// Live reads the backend's JSON SSE stream for one team.
//
// This is the only file that has to change when the backend lands. It is
// written and compiled now, against the envelope the plan specifies, so the
// swap is a flag flip rather than a porting exercise. The backend has not been
// built yet, so treat the field names here as the proposal the frontend is
// making, not as something already agreed.
//
// Reconnection is exact rather than best-effort: the cursor advances only on
// events actually handed downstream, and every reconnect re-issues
// ?after=<cursor>. A dropped connection therefore loses nothing and, because
// the store also rejects sequences it has already applied, replays nothing
// twice even if the backend is generous about the boundary.
type Live struct {
	// BaseURL is the backend root, with or without the /v1 suffix.
	BaseURL string
	Slug    string
	Client  *http.Client
	// Backoff is the pause before reconnecting. It does not escalate: the
	// backend is on the same cluster and a stream closed by an idle proxy is
	// the common case, not an outage.
	Backoff time.Duration
	Logger  *slog.Logger
}

// Name implements Feed.
func (l *Live) Name() string { return "live:" + l.BaseURL }

func (l *Live) root() string {
	trimmed := strings.TrimRight(l.BaseURL, "/")
	if strings.HasSuffix(trimmed, "/v1") {
		return trimmed
	}
	return trimmed + "/v1"
}

// Stream implements Feed.
func (l *Live) Stream(ctx context.Context, after int64) (<-chan model.Event, error) {
	client := l.Client
	if client == nil {
		// No overall timeout: this request is meant to stay open.
		client = &http.Client{}
	}
	backoff := l.Backoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	log := l.Logger
	if log == nil {
		log = slog.Default()
	}

	out := make(chan model.Event, 64)
	go func() {
		defer close(out)
		cursor := after
		for ctx.Err() == nil {
			n, err := l.pump(ctx, client, cursor, out)
			cursor = max64(cursor, n)
			if err != nil && ctx.Err() == nil {
				log.Warn("metiche stream dropped", "slug", l.Slug, "cursor", cursor, "err", err)
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
func (l *Live) pump(ctx context.Context, client *http.Client, after int64, out chan<- model.Event) (int64, error) {
	url := fmt.Sprintf("%s/teams/%s/stream?after=%d", l.root(), l.Slug, after)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return after, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return after, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return after, fmt.Errorf("stream %s: %s", url, resp.Status)
	}

	cursor := after
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var data strings.Builder
	flush := func() error {
		defer data.Reset()
		if data.Len() == 0 {
			return nil
		}
		var ev model.Event
		if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
			// One malformed frame must not kill the stream.
			return nil
		}
		if ev.Sequence <= cursor {
			return nil
		}
		cursor = ev.Sequence
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- ev:
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
				cursor = max64(cursor, n-1)
			}
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
