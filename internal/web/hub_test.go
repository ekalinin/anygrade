package web

import (
	"testing"

	"github.com/ekalinin/anygrade/internal/queue"
)

func TestHubRouting(t *testing.T) {
	h := NewHub()
	bySub, cancelSub := h.SubscribeSubmission(7)
	byUsr, cancelUsr := h.SubscribeUser(1)
	defer cancelUsr()

	h.Publish(queue.Event{SubID: 7, UserID: 1, TaskID: "t1", Status: "running"})
	if ev := <-bySub; ev.Status != "running" {
		t.Fatalf("submission channel: %+v", ev)
	}
	if ev := <-byUsr; ev.SubID != 7 {
		t.Fatalf("user channel: %+v", ev)
	}

	// Unrelated ids do not deliver.
	h.Publish(queue.Event{SubID: 8, UserID: 2, Status: "done"})
	select {
	case ev := <-bySub:
		t.Fatalf("unexpected delivery: %+v", ev)
	default:
	}

	// Cancel unsubscribes.
	cancelSub()
	h.Publish(queue.Event{SubID: 7, UserID: 1, Status: "done"})
	if ev := <-byUsr; ev.Status != "done" {
		t.Fatalf("user channel after cancelSub: %+v", ev)
	}
}

// TestHubOverflowDoesNotBlock: a full subscriber buffer drops events instead
// of stalling the publisher (queue workers must never wait on the UI).
func TestHubOverflowDoesNotBlock(t *testing.T) {
	h := NewHub()
	ch, cancel := h.SubscribeSubmission(1)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			h.Publish(queue.Event{SubID: 1, UserID: 1, Status: "running", TaskID: string(rune('a' + i%26))})
		}
	}()
	<-done // must complete despite nobody reading ch
	if len(ch) != cap(ch) {
		t.Fatalf("buffer: len=%d cap=%d", len(ch), cap(ch))
	}
}
