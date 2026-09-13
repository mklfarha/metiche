package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// settledConflictLimit bounds the settled conflicts a board shows. The
// Settled section is history for a glance, not an audit log.
const settledConflictLimit = 50

type settledConflictsWire struct {
	Conflicts []conflictJSON `json:"conflicts"`
}

// settledWire reads the team's resolved conflicts, to sit next to the open
// ones the team snapshot carries.
//
// The team snapshot deliberately carries only the open conflicts — what a
// person still has to act on — so a conflict the agents settled would
// otherwise vanish from the board the instant it closed, instead of moving to
// the Settled section with the explanation of how.
//
// BEST-EFFORT, on purpose. It is read after the snapshot, so it can only be
// ahead of the cursor, which is harmless (see Snapshot); a conflict that closed
// between the two reads arrives in both lists, and the store keeps the later,
// resolved copy because it is keyed. Any failure — including a 404 from a
// backend that predates the endpoint — returns nothing: it never fails the
// snapshot and never reports the team gone, because the team read that decides
// that already answered 200.
func (l *Live) settledWire(ctx context.Context, base string) []conflictJSON {
	var wire settledConflictsWire
	target := fmt.Sprintf("%s/conflicts?status=resolved&limit=%d", base, settledConflictLimit)
	if err := l.getJSONQuietly(ctx, target, &wire); err != nil {
		l.log().Debug("settled conflicts unavailable; showing the open ones only", "slug", l.Slug, "err", err)
		return nil
	}
	return wire.Conflicts
}

// getJSONQuietly is getJSON without the not-found report: a failure here is
// an error to ignore, never a verdict on the team.
func (l *Live) getJSONQuietly(ctx context.Context, target string, v any) error {
	timeout := l.RequestTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := l.request(ctx, target)
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
