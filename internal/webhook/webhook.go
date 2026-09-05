// Package webhook delivers anygrade events to one course-wide HTTP endpoint
// (SPEC §16). It is the only outbound HTTP in the binary, which is why every
// part of it is a bound: a scheme allowlist and an address policy on the
// target, a deadline on every attempt, no redirects, a capped read of the
// response, and a capped number of attempts.
//
// The target comes from course.yaml, so a teacher can move it with a push; the
// signing secret comes from the environment (SPEC §11), because course.yaml is
// inside the repo every student clones. Nothing here may influence a
// submission: the deliverer never touches the store, and a receiver that is
// down only costs log lines.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	// SecretEnv holds the HMAC-SHA256 key every delivery is signed with. It is
	// a credential, so it comes from the server's environment and never from
	// the course repo - the same rule ANYGRADE_HIDDEN_GIT_TOKEN follows. It is
	// also the switch that permits outbound traffic at all: with it unset the
	// server builds no deliverer and makes no request, whatever course.yaml says.
	SecretEnv = "ANYGRADE_WEBHOOK_SECRET"

	// AllowPrivateEnv, when set to anything non-empty, lifts the refusal to
	// connect to loopback and private addresses. It is the operator's switch,
	// not the teacher's, because the teacher who edits course.yaml is not
	// necessarily the administrator of the machine (the same asymmetry
	// ANYGRADE_HIDDEN_LOCAL_ROOTS exists for). Set it to run a local relay -
	// a Telegram or mail bridge on 127.0.0.1 is the intended use.
	AllowPrivateEnv = "ANYGRADE_WEBHOOK_ALLOW_PRIVATE"
)

// EventCompleted is the one event kind delivered today: a submission reached a
// state it will never leave. The kind travels in the payload and in a header so
// a receiver can route on it without parsing, and so a second kind (a teacher
// score override) can be added later without breaking the first.
const EventCompleted = "submission.completed"

// maxResponseBody bounds what one delivery reads back. The response is never
// used for anything, but draining a little of it lets the connection be reused,
// and a receiver answering with a gigabyte must not cost the server memory.
const maxResponseBody = 4 << 10

// Event is one thing that happened, in the shape the payload needs. The login
// is deliberately absent: resolving it is a database read, and Send runs on a
// worker goroutine that may not do one.
type Event struct {
	Kind    string // EventCompleted
	SubID   int64
	UserID  int64
	TaskID  string
	Status  string
	Raw     float64
	Penalty float64
	Final   float64
}

// payload is the delivered JSON. It carries identifiers and scores and nothing
// else: check excerpts, log paths and worker notes are teacher-only material
// (SPEC §14), and a webhook receiver is whatever the teacher pointed it at.
type payload struct {
	Event          string  `json:"event"`
	SentAt         string  `json:"sent_at"`
	SubmissionID   int64   `json:"submission_id"`
	Student        string  `json:"student"`
	Task           string  `json:"task"`
	Status         string  `json:"status"`
	Score          float64 `json:"score"`
	RawScore       float64 `json:"raw_score"`
	PenaltyPercent float64 `json:"penalty_percent"`
}

// delivery is one queued event with its target pinned. The URL is resolved
// when the event is queued, not per attempt: a teacher push landing mid-retry
// would otherwise send the tail of one event's attempts to a different host
// than its head.
type delivery struct {
	ev  Event
	url string
}

// Sink is the deliverer. Send hands it an event and returns; Run does the
// posting on its own goroutines. The zero value is not usable - Target, Secret
// and Login are required - and a Sink without a Secret stays silent, which is
// what keeps "no configuration" and "no outbound request" the same thing.
type Sink struct {
	// Target returns the endpoint for the next event, "" when the course
	// configures none. It is read per event rather than fixed at startup
	// because a teacher metadata push swaps the course snapshot, and the
	// target has to follow it like every other course.yaml key.
	Target func() string
	// Secret is the HMAC-SHA256 key; empty disables delivery entirely.
	Secret []byte
	// Login resolves a user id to the login the payload names. It runs on a
	// delivery goroutine, never on a worker's.
	Login func(ctx context.Context, userID int64) (string, error)
	// AllowPrivate permits targets that resolve to loopback, link-local or
	// private addresses (AllowPrivateEnv).
	AllowPrivate bool
	Log          *slog.Logger

	Timeout     time.Duration // per attempt; default 10s
	MaxAttempts int           // default 5
	BackoffBase time.Duration // default 1s
	BackoffCap  time.Duration // default 30s
	QueueSize   int           // pending events held before dropping; default 256
	Workers     int           // concurrent deliveries; default 2

	once   sync.Once
	events chan delivery
	client *http.Client
	// mu guards stopped against the queue it closes. Run sets it under the
	// write lock after its workers are gone and before it drains, so a Send
	// holding the read lock has its event in the channel by the time drain
	// looks, and every Send after that finds the sink closed. Nothing may land
	// in a channel with no reader left: that is the one loss that would be
	// silent, and a delivery lost at shutdown has to be a line in the log.
	mu      sync.RWMutex
	stopped bool
}

func (s *Sink) init() {
	s.once.Do(func() {
		if s.Timeout <= 0 {
			s.Timeout = 10 * time.Second
		}
		if s.MaxAttempts <= 0 {
			s.MaxAttempts = 5
		}
		if s.BackoffBase <= 0 {
			s.BackoffBase = time.Second
		}
		if s.BackoffCap <= 0 {
			s.BackoffCap = 30 * time.Second
		}
		if s.QueueSize <= 0 {
			s.QueueSize = 256
		}
		if s.Workers <= 0 {
			s.Workers = 2
		}
		if s.Log == nil {
			s.Log = slog.Default()
		}
		if s.Login == nil {
			// Defensive: a miswired sink must fail one delivery with a log
			// line, not panic a delivery goroutine and take the server with it.
			s.Login = func(context.Context, int64) (string, error) {
				return "", errors.New("no login resolver configured")
			}
		}
		s.events = make(chan delivery, s.QueueSize)
		s.client = newClient(s.AllowPrivate)
	})
}

// Send queues one event and returns immediately. It never blocks: it is called
// from a queue worker right after the submission row was written, and a slow or
// hung receiver may not hold up the next submission. A full queue therefore
// drops the event it was just handed, with a log line: the alternative is
// stalling grading behind an endpoint nobody here controls.
func (s *Sink) Send(ev Event) {
	s.init()
	// Two independent reasons to make no request at all. Both are belt and
	// braces: the composition root does not build a Sink without a secret, and
	// a course without `webhook.url` yields an empty target.
	if len(s.Secret) == 0 || s.Target == nil {
		return
	}
	target := s.Target()
	if target == "" {
		return
	}
	s.mu.RLock()
	stopped, full := s.stopped, false
	if !stopped {
		select {
		case s.events <- delivery{ev: ev, url: target}:
		default:
			full = true
		}
	}
	s.mu.RUnlock()

	// Outside the lock: the log write is I/O, and nothing about it belongs in
	// the window Run waits on to stop.
	switch {
	case stopped:
		s.Log.Warn("webhook event dropped after shutdown",
			"event", ev.Kind, "submission", ev.SubID)
	case full:
		s.Log.Warn("webhook queue is full, event dropped",
			"event", ev.Kind, "submission", ev.SubID)
	}
}

// Run delivers queued events until ctx is canceled, then reports whatever was
// still waiting. It returns promptly on cancellation: an attempt in flight is
// aborted rather than waited out, since the events are not persisted and a
// grace period would only trade a bounded shutdown for an unbounded one.
func (s *Sink) Run(ctx context.Context) {
	s.init()
	var wg sync.WaitGroup
	for range s.Workers {
		wg.Go(func() { s.loop(ctx) })
	}
	wg.Wait()
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.drain()
}

func (s *Sink) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-s.events:
			s.deliver(ctx, d)
		}
	}
}

// drain accounts for the events the shutdown lost. They are held in memory
// only, so a stop discards them; discarding them silently is what would make
// the resulting gap in the receiver's history unexplainable.
func (s *Sink) drain() {
	for {
		select {
		case d := <-s.events:
			s.Log.Warn("webhook event abandoned at shutdown",
				"event", d.ev.Kind, "submission", d.ev.SubID)
		default:
			return
		}
	}
}

// deliver posts one event, retrying with exponential backoff until it is
// accepted, permanently refused, or out of attempts. Every exit that is not a
// success writes a log line: the submission is already final in the database,
// so this is the only place the loss can be noticed.
func (s *Sink) deliver(ctx context.Context, d delivery) {
	body, err := s.body(ctx, d.ev)
	if err != nil {
		// A shutdown reaches the login lookup first, so the failure has to be
		// reported for what it is rather than as a broken payload.
		if ctx.Err() != nil {
			s.Log.Warn("webhook event abandoned at shutdown",
				"event", d.ev.Kind, "submission", d.ev.SubID)
			return
		}
		s.Log.Warn("webhook payload could not be built, event dropped",
			"event", d.ev.Kind, "submission", d.ev.SubID, "err", err)
		return
	}
	for attempt := 1; ; attempt++ {
		status, err := s.post(ctx, d, body, attempt)
		switch {
		case err == nil && status/100 == 2:
			s.Log.Debug("webhook delivered",
				"event", d.ev.Kind, "submission", d.ev.SubID, "attempt", attempt)
			return
		case ctx.Err() != nil:
			s.Log.Warn("webhook delivery abandoned at shutdown",
				"event", d.ev.Kind, "submission", d.ev.SubID, "attempt", attempt)
			return
		case !retryable(status, err):
			s.Log.Warn("webhook delivery refused, not retrying",
				"event", d.ev.Kind, "submission", d.ev.SubID, "status", status)
			return
		case attempt >= s.MaxAttempts:
			s.Log.Warn("webhook delivery failed, giving up",
				"event", d.ev.Kind, "submission", d.ev.SubID,
				"attempts", attempt, "status", status, "err", err)
			return
		}
		delay := min(s.BackoffBase<<(attempt-1), s.BackoffCap)
		// ±10% jitter, like the queue's own backoff: a receiver that came back
		// up should not be hit by every pending delivery in the same instant.
		delay += time.Duration((rand.Float64() - 0.5) * 0.2 * float64(delay))
		select {
		case <-ctx.Done():
			s.Log.Warn("webhook delivery abandoned at shutdown",
				"event", d.ev.Kind, "submission", d.ev.SubID, "attempt", attempt)
			return
		case <-time.After(delay):
		}
	}
}

// body resolves the student's login and encodes the payload once, so every
// attempt of one event sends byte-identical content.
func (s *Sink) body(ctx context.Context, ev Event) ([]byte, error) {
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	login, err := s.Login(lctx, ev.UserID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(payload{
		Event:          ev.Kind,
		SentAt:         time.Now().UTC().Format(time.RFC3339),
		SubmissionID:   ev.SubID,
		Student:        login,
		Task:           ev.TaskID,
		Status:         ev.Status,
		Score:          ev.Final,
		RawScore:       ev.Raw,
		PenaltyPercent: ev.Penalty,
	})
}

// post makes one attempt. The signature is recomputed per attempt over a fresh
// timestamp, so a receiver enforcing a replay window sees a live signature on a
// redelivery instead of an expired one.
func (s *Sink) post(ctx context.Context, d delivery, body []byte, attempt int) (int, error) {
	rctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "anygrade")
	req.Header.Set("X-Anygrade-Event", d.ev.Kind)
	req.Header.Set("X-Anygrade-Attempt", strconv.Itoa(attempt))
	req.Header.Set("X-Anygrade-Timestamp", ts)
	req.Header.Set("X-Anygrade-Signature", "v1="+Sign(s.Secret, ts, body))

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, scrubURL(err)
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, maxResponseBody)
	return resp.StatusCode, nil
}

// scrubURL keeps the cause of a transport failure without the target it names.
// http.Client wraps every failure in a *url.Error whose message quotes the
// whole URL, path and query included, and that message would otherwise reach
// the give-up log line - while every diagnostic on the way in deliberately
// withholds the URL, because a webhook path is itself often the credential.
// The wrapped cause still names the address and the syscall, which is what the
// operator needs.
func scrubURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// retryable reports whether another attempt could plausibly succeed. Transport
// faults and 5xx are the receiver's problem and pass; 408 and 429 ask for
// exactly that. Any other 4xx says this payload is wrong for this endpoint, and
// repeating it only burns the budget an outage would need.
func retryable(status int, err error) bool {
	if err != nil {
		return true
	}
	if status >= 500 {
		return true
	}
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests
}

// Sign returns the hex HMAC-SHA256 of "<timestamp>.<body>" - the scheme a
// receiver has to reproduce. The timestamp is inside the signed material, not
// beside it, so a delivery captured today cannot be replayed tomorrow with its
// header rewritten: the receiver rejects a timestamp outside its tolerance, and
// changing the timestamp invalidates the signature.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
