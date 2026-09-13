package hub

import (
	"testing"

	"github.com/mklfarha/metiche/frontend/internal/view"
)

// TestCloseEndsSubscriptionsAndRefusesNewOnes: Close is how a board taken down
// gets its open browsers off it.
func TestCloseEndsSubscriptionsAndRefusesNewOnes(t *testing.T) {
	h := New("x", "X", view.Renderer{}, quietLogger())
	a, b := h.Subscribe(), h.Subscribe()
	h.Close()
	for _, s := range []*Subscriber{a, b} {
		if _, open := <-s.C; open {
			t.Fatal("a subscription survived Close")
		}
	}
	if h.Subscribers() != 0 || !h.Closed() {
		t.Fatalf("subscribers = %d, closed = %v", h.Subscribers(), h.Closed())
	}
	late := h.Subscribe()
	if _, open := <-late.C; open {
		t.Fatal("Subscribe on a closed hub returned an open channel")
	}
	h.Unsubscribe(a)
	h.Unsubscribe(late)
	h.Close() // twice is fine
}
