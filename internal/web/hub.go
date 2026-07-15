package web

import (
	"sync"

	"github.com/ekalinin/anygrade/internal/queue"
)

// Hub is the in-process pub/sub between the queue/intake writers and SSE
// handlers. Delivery is best-effort by contract (queue.Publisher): a full
// subscriber buffer drops the event, and the UI reconciles terminal state
// with a DB fetch on its `done` event.
type Hub struct {
	mu    sync.Mutex
	bySub map[int64]map[chan queue.Event]struct{}
	byUsr map[int64]map[chan queue.Event]struct{}
}

func NewHub() *Hub {
	return &Hub{
		bySub: map[int64]map[chan queue.Event]struct{}{},
		byUsr: map[int64]map[chan queue.Event]struct{}{},
	}
}

// Publish implements queue.Publisher. It never blocks: publishers run on
// worker goroutines between DB writes.
func (h *Hub) Publish(ev queue.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.bySub[ev.SubID] {
		select {
		case ch <- ev:
		default:
		}
	}
	for ch := range h.byUsr[ev.UserID] {
		select {
		case ch <- ev:
		default:
		}
	}
}

// SubscribeSubmission delivers events of one submission; call cancel when
// the SSE connection ends.
func (h *Hub) SubscribeSubmission(id int64) (<-chan queue.Event, func()) {
	return h.subscribe(h.bySub, id)
}

// SubscribeUser delivers every event of one user's submissions (dashboard).
func (h *Hub) SubscribeUser(id int64) (<-chan queue.Event, func()) {
	return h.subscribe(h.byUsr, id)
}

func (h *Hub) subscribe(m map[int64]map[chan queue.Event]struct{}, id int64) (<-chan queue.Event, func()) {
	ch := make(chan queue.Event, 16)
	h.mu.Lock()
	if m[id] == nil {
		m[id] = map[chan queue.Event]struct{}{}
	}
	m[id][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(m[id], ch)
		if len(m[id]) == 0 {
			delete(m, id)
		}
		h.mu.Unlock()
	}
}
