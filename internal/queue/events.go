package queue

// Event is one submission state change, published for live UI updates.
// Delivery is best-effort: consumers must reconcile terminal state against
// the DB (the web layer re-fetches on its terminal event).
type Event struct {
	SubID  int64
	UserID int64
	TaskID string
	Status string // store.Status* value just written
}

// Publisher receives events. Implementations must never block: they are
// called on worker goroutines between DB writes.
type Publisher interface {
	Publish(Event)
}
