// Package publish is the seam between whatever COMMITS a team event and
// whatever DELIVERS it to a browser.
//
// It exists as its own package, holding one interface and one struct, for a
// single reason: the write path (the MCP tool surface) must be able to announce
// "I just committed sequence N for team T" without importing the streaming
// machinery — the hub, the SSE handler, chi, the database tailer. Keeping the
// seam this thin means swapping the transport later (a real bus, a websocket
// gateway, a cross-region broker) touches exactly one file, and the tool code
// that calls Publish never changes.
//
// It deliberately has no dependency beyond a uuid type: no database, no HTTP,
// no logger, no configuration.
package publish

import "github.com/gofrs/uuid"

// Event is the notification that a team event has been committed and is now
// visible to any reader.
//
// It carries the CURSOR, not the content. That is the important design choice:
// the fast in-process path and the database tailer must deliver byte-identical
// frames to a browser, and the only way to guarantee that is for both to build
// the frame from the same read of the same row. If Publish carried a
// pre-rendered payload, the two paths would be two renderers, and they would
// drift — silently, and only for the events that happen to take the fast path.
//
// The cost is one indexed point read on the fast path, which is what "≈2ms"
// already budgets for.
type Event struct {
	// TeamUUID is the team whose subscribers should be woken.
	TeamUUID uuid.UUID
	// Sequence is the team-scoped monotonic sequence of the committed event.
	// Subscribers dedupe on it, which is what makes it safe for the same event
	// to arrive by both the fast path and the tailer.
	Sequence int64
}

// Publisher is called by the write path immediately AFTER its transaction
// commits — never inside it. Publishing before the commit lands would tell a
// reader to go and look at a row that is not visible yet, and the reader would
// find nothing and move its cursor past it.
//
// Implementations must not block the caller: a tool response should never wait
// on fan-out. The hub's implementation is a non-blocking channel send.
type Publisher interface {
	Publish(ev Event)
}

// PublisherFunc adapts a plain function to Publisher.
type PublisherFunc func(ev Event)

// Publish implements Publisher.
func (f PublisherFunc) Publish(ev Event) { f(ev) }

// Discard is the no-op Publisher. It is what a process with no subscribers
// attached (a worker-only role, a test, a CLI) should be wired with, so the
// write path never has to branch on whether streaming is enabled.
type Discard struct{}

// Publish implements Publisher and does nothing.
func (Discard) Publish(Event) {}

var _ Publisher = Discard{}
var _ Publisher = PublisherFunc(nil)
