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
// and wrapped as ErrNotFound; any other failure is neither. The stream keeps
// reconnecting either way — stopping is the owner's call.
func TestLiveReportsNotFound(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusNotFound)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such team", int(status.Load()))
	}))
	defer srv.Close()

	var reports atomic.Int64
	l := &Live{BaseURL: srv.URL, Slug: "gone", Client: srv.Client(), Backoff: time.Millisecond}
	l.OnNotFound(func() { reports.Add(1) })

	if _, err := l.Snapshot(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("snapshot 404: err = %v, want ErrNotFound", err)
	}
	if reports.Load() != 1 {
		t.Fatalf("reports after snapshot 404 = %d, want 1", reports.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := l.Stream(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for reports.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if reports.Load() < 4 {
		t.Fatalf("the stream did not keep reconnecting and reporting: %d", reports.Load())
	}
	cancel()
	for range ch {
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
