package queue

// Completion is one submission reaching a state it will never leave: graded, or
// terminally failed, or canceled by a teacher. Every submission the policy
// admitted produces exactly one; a submission the policy rejected produces none,
// because it never entered the queue and nothing is waiting for its outcome.
//
// It is deliberately not an Event. The live-UI stream carries every
// intermediate status and is best effort by contract, while this is the fact an
// external system synchronizes on - and it has to carry the scores, which the
// UI re-fetches from the database and Event therefore never needed.
type Completion struct {
	SubID   int64
	UserID  int64
	TaskID  string
	Status  string // the status actually written to the row
	Raw     float64
	Penalty float64
	Final   float64
}

// Notifier receives completions. Like Publisher, implementations must never
// block: they are called on worker goroutines, and a receiver that is down may
// not hold up the next submission. What an implementation does with a
// completion - deliver it, retry it, give up on it - is entirely its own
// business, because nothing it does may reach the submission's status: grading
// does not depend on anybody being up (SPEC §16).
type Notifier interface {
	Completed(Completion)
}
