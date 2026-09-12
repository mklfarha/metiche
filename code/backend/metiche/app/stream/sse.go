package stream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/core"
)

// keepaliveInterval is how often a comment frame is written on an idle stream.
//
// Proxies and load balancers cull a connection that has been silent for a
// while, and a browser cannot tell a quiet team from a dead socket. A comment
// line is the cheapest thing SSE defines: clients ignore it, intermediaries
// count it as traffic.
const keepaliveInterval = 20 * time.Second

// StreamPath is the route this package serves. Exported so whoever mounts it
// does not have to retype it, and so a test can assert it has not drifted.
const StreamPath = "/v1/teams/{slug}/stream"

// Server is the HTTP half of the stream.
type Server struct {
	hub    *Hub
	lookup TeamLookup
	logger *zap.Logger

	// keepalive is a field rather than a constant only so a test can observe a
	// comment frame without waiting twenty seconds for one.
	keepalive time.Duration
}

// NewServer builds the SSE handler over a hub and a team lookup. Both are
// parameters rather than constructed here so a test can drive the exact
// reconnect semantics with a faked event source and no database.
func NewServer(hub *Hub, lookup TeamLookup, logger *zap.Logger) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Server{hub: hub, lookup: lookup, logger: logger, keepalive: keepaliveInterval}
}

// RegisterOn mounts the stream route on r.
//
// Spell the full path including /v1: r is the server's ROOT router and the
// generated CRUD already occupies /v1, so re-mounting it would panic at
// startup.
func (s *Server) RegisterOn(r chi.Router) {
	r.Get(StreamPath, s.Handle)
}

// Register wires the stream into the REST server and returns the Hub.
//
// The returned Hub is also a publish.Publisher: hand it to the write path and
// every commit reaches the browsers attached to this process in milliseconds
// instead of on the next 250ms tick. Nothing breaks if it is not handed over —
// the tailer still delivers everything — which is the property that makes the
// fast path an optimisation rather than a dependency.
//
// The caller owns the Hub's lifetime and should Close it on shutdown.
func Register(r chi.Router, coreImpl *core.Implementation, logger *zap.Logger) *Hub {
	db := coreImpl.DB()
	hub := NewHub(NewDBSource(db), logger)
	NewServer(hub, NewDBTeamLookup(db), logger).RegisterOn(r)
	return hub
}

// Handle serves GET /v1/teams/{slug}/stream?after=N.
//
// The contract, which the board depends on being exact: everything with a
// sequence strictly greater than after is delivered, once each, in order. A
// client that drops off for thirty seconds and reconnects with its last good
// sequence receives precisely what it missed — no hole to reason about, no
// duplicate to filter.
func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
	team, err := s.lookup(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		if errors.Is(err, ErrTeamNotFound) {
			http.Error(w, "no such team", http.StatusNotFound)
			return
		}
		// The error itself is not echoed: it can carry driver text, and driver
		// text can carry a DSN.
		s.logger.Warn("resolving team for stream failed", zap.Error(err))
		http.Error(w, "could not resolve team", http.StatusInternalServerError)
		return
	}

	after, err := cursorFrom(r)
	if err != nil {
		http.Error(w, "after must be a non-negative integer sequence", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nginx buffers a proxied response by default, which holds every frame
	// until the buffer fills: the stream looks dead, then arrives in a lump.
	// This is what turns that off at the ingress.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// SUBSCRIBE BEFORE REPLAYING. The other order has a hole in it: an event
	// committed after the replay query has run but before the subscription
	// exists reaches neither, and the client sits on a permanent gap it can
	// only escape by reloading. This way the two overlap instead, and the
	// overlap is dropped below by comparing against the highest sequence
	// already written.
	frames, release := s.hub.Subscribe(team.UUID, after)
	defer release()

	highest, err := s.replay(r.Context(), team.UUID, after, w, flusher)
	if err != nil {
		if r.Context().Err() == nil {
			s.logger.Warn("stream replay failed; the client will resync", zap.Error(err))
		}
		return
	}

	ticker := time.NewTicker(s.keepalive)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The client went away. Returning runs the deferred release, which
			// closes the subscription and — if it was the last one — stops the
			// team's tailing goroutine. Nothing leaks.
			return

		case f, ok := <-frames:
			if !ok {
				// The hub was closed.
				return
			}
			if f.Sequence <= highest {
				// Already sent during replay, or already sent live. This is the
				// per-connection half of the deduplication; the hub does the
				// other half across the two delivery paths.
				continue
			}
			if err := writeFrame(w, flusher, f); err != nil {
				return
			}
			highest = f.Sequence

		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// replay writes every event after the client's cursor and returns the highest
// sequence written.
//
// It loops rather than issuing one query, because the source caps a batch: a
// client reconnecting after a long absence, or loading a team with history,
// must still receive all of it before the live frames start.
func (s *Server) replay(ctx context.Context, teamUUID uuid.UUID, after int64, w http.ResponseWriter, flusher http.Flusher) (int64, error) {
	highest := after
	for {
		batch, err := s.hub.src.FramesAfter(ctx, teamUUID, highest, batchLimit)
		if err != nil {
			return highest, err
		}
		for _, f := range batch {
			if err := writeFrame(w, flusher, f); err != nil {
				return highest, err
			}
			highest = f.Sequence
		}
		if len(batch) < batchLimit {
			return highest, nil
		}
	}
}

// cursorFrom reads the resume cursor.
//
// ?after= is the explicit form the board uses, because it loads a snapshot
// first and resumes from the sequence that snapshot reported. Last-Event-ID is
// the browser's own automatic reconnect, which EventSource sends without being
// asked — honouring it means a dropped connection recovers exactly even for a
// client that never wrote a line of reconnect logic.
func cursorFrom(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, errBadCursor
	}
	return v, nil
}

var errBadCursor = errors.New("bad cursor")
