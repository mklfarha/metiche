// Package feed is the frontend's only door to event data.
//
// There are two implementations behind one interface: Fixture replays a JSONL
// recording on a timer, Live reads the backend's JSON SSE stream. Everything
// downstream — the store, the hub, the templates — is written against the
// interface and cannot tell which one it is talking to, so moving from the
// recorded feed to the real backend is a flag, not a rewrite.
//
// The contract both sides honour:
//
//   - events arrive in strictly increasing Sequence order;
//   - Stream(after) yields only events with Sequence > after, which is what
//     makes reconnection lossless and duplicate-free;
//   - the channel is closed when the feed is exhausted or the context ends.
package feed

import (
	"context"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// Feed produces a team's events.
type Feed interface {
	// Name identifies the implementation for the /healthz page and logs.
	Name() string
	// Stream returns a channel of events with Sequence > after. The channel is
	// closed when the feed ends or ctx is cancelled.
	Stream(ctx context.Context, after int64) (<-chan model.Event, error)
}

// Snapshotter is the other half of a live feed: the state itself, read whole
// at one instant, with the cursor to resume streaming from.
//
// It is a separate, OPTIONAL interface rather than part of Feed because the
// two implementations genuinely differ. A fixture recording carries the whole
// world in its event payloads, so folding the log is the state and there is
// nothing to snapshot. The real backend's frames deliberately do not: they say
// what changed, and the read API says what things now are. A consumer asks
// whether its feed can do this and behaves accordingly — see hub.Hub.Run.
type Snapshotter interface {
	// Snapshot reads the whole team at one instant. The returned ResumeFrom is
	// the cursor to pass to Stream so that nothing is missed and nothing
	// arrives twice.
	Snapshot(ctx context.Context) (model.TeamState, error)
}
