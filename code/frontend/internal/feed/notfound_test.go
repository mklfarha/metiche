package feed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestLiveReportsNotFound: a 404 on the snapshot or the stream is reported
// and wrapped as ErrNotFound; any other failure is neither. A 404 on the
// stream is TERMINAL: the channel closes and the backend is not asked again.
func TestLiveReportsNotFound(t *testing.T) {
	var status, streamHits atomic.Int64
	status.Store(http.StatusNotFound)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/teams/gone/stream" {
			streamHits.Add(1)
		}
		http.Error(w, "no such team", int(status.Load()))
	}))
	defer srv.Close()

	var reports atomic.Int64
	l := &Live{BaseURL: srv.URL, Slug: "gone", Client: srv.Client(), Backoff: time.Millisecond, Logger: quietLog()}
	l.OnNotFound(func() { reports.Add(1) })

	if _, err := l.Snapshot(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("snapshot 404: err = %v, want ErrNotFound", err)
	}
	if reports.Load() != 1 {
		t.Fatalf("reports after snapshot 404 = %d, want 1", reports.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := l.Stream(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("an event arrived from a 404")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end on a 404")
	}
	time.Sleep(50 * time.Millisecond) // ~50 backoffs, had it kept reconnecting
	if got := streamHits.Load(); got != 1 {
		t.Fatalf("stream requests after a 404 = %d, want 1", got)
	}
	if reports.Load() != 2 {
		t.Fatalf("reports = %d, want 2 (snapshot, stream)", reports.Load())
	}

	status.Store(http.StatusInternalServerError)
	before := reports.Load()
	if _, err := l.Snapshot(context.Background()); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("snapshot 500: err = %v, want a non-ErrNotFound error", err)
	}
	if reports.Load() != before {
		t.Fatal("a 500 was reported as not found")
	}
}

// TestLiveStreamKeepsReconnectingOnOtherErrors: a 500 — or a 401 on a feed
// with no browser session — is an outage, not a verdict, so the stream keeps
// trying. With a browser session a 401 means the session is gone: terminal.
func TestLiveStreamKeepsReconnectingOnOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		session  string
		terminal bool
	}{
		{"500", http.StatusInternalServerError, "", false},
		{"401 without a session", http.StatusUnauthorized, "", false},
		{"401 with a session", http.StatusUnauthorized, "mbs_fake-session-0001", true},
		{"404 with a session", http.StatusNotFound, "mbs_fake-session-0001", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				http.Error(w, "nope", tc.status)
			}))
			defer srv.Close()
			var reports atomic.Int64
			l := &Live{BaseURL: srv.URL, Slug: "x", BrowserSession: tc.session, Client: srv.Client(),
				Backoff: time.Millisecond, Logger: quietLog()}
			l.OnNotFound(func() { reports.Add(1) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, _ := l.Stream(ctx, 0)

			if tc.terminal {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("not terminal")
				}
				if hits.Load() != 1 || reports.Load() != 1 {
					t.Fatalf("hits=%d reports=%d, want 1 and 1", hits.Load(), reports.Load())
				}
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for hits.Load() < 5 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if hits.Load() < 5 {
				t.Fatalf("stopped reconnecting after %d", hits.Load())
			}
			if reports.Load() != 0 {
				t.Fatalf("reported %d times as not found", reports.Load())
			}
		})
	}
}
