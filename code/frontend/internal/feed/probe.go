package feed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ProbeResult is what the backend said about one slug.
type ProbeResult int

const (
	// ProbeFound means GET /v1/teams/{slug} answered 200: the team exists and
	// this caller may read it.
	ProbeFound ProbeResult = iota + 1
	// ProbeNotFound means 404. The backend deliberately answers the same way
	// for "no such team" and "private and you may not read it" (app/authz), and
	// so does the board: nothing here tries to tell them apart.
	ProbeNotFound
)

// ProbeTeam asks the backend whether ANYBODY may see slug: the read carries no
// credential at all, so a 200 means the team is public. That is what lets
// discovery remember its answers in a cache every anonymous request reads —
// nothing a probe learns depends on who asked. A signed-in viewer's access is
// asked separately (BrowserClient.Access) and never cached.
//
// It is one snapshot read and nothing else — no stream, no goroutine left
// behind — so a caller can decide whether a feed is worth creating BEFORE it
// creates one. Anything other than 200 or 404 is returned as an error, which
// the caller must not confuse with "does not exist": a backend outage is not a
// verdict about the team.
//
// The body is discarded unread. On 200 it is the snapshot, which the feed will
// read again for itself; on anything else it is a problem document, which is
// never logged for the reason getJSON gives.
func ProbeTeam(ctx context.Context, client *http.Client, baseURL, slug string) (ProbeResult, error) {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	l := &Live{BaseURL: baseURL}
	req, err := l.request(ctx, l.root()+"/teams/"+url.PathEscape(slug))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		return ProbeFound, nil
	case http.StatusNotFound:
		return ProbeNotFound, nil
	default:
		return 0, fmt.Errorf("GET %s: %s", req.URL.Path, resp.Status)
	}
}
