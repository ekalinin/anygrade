package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSecret = "topsecret"

// logBuf collects slog output written from several delivery goroutines.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type request struct {
	body    []byte
	headers http.Header
}

// receiver is an httptest endpoint that records what it was sent and answers
// with a fixed status. With hold set, every handler blocks until it is closed,
// which is how "the receiver is down" is made deterministic instead of timed.
type receiver struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []request
	hold chan struct{}
}

func newReceiver(t *testing.T, status int) *receiver {
	t.Helper()
	r := &receiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, request{body: body, headers: req.Header.Clone()})
		hold := r.hold
		r.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-req.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.Close)
	return r
}

// block makes every handler wait until the test ends, which is how "the
// receiver is down" is expressed without depending on a timer.
func (r *receiver) block(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	r.hold = make(chan struct{})
	hold := r.hold
	r.mu.Unlock()
	t.Cleanup(func() { close(hold) })
}

func (r *receiver) all() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reqs)
}

func (r *receiver) count() int { return len(r.all()) }

// newSink returns a running sink pointed at url. AllowPrivate is on because the
// httptest receiver listens on 127.0.0.1; the policy itself is exercised with
// it off in TestPrivateTargetIsRefused.
func newSink(t *testing.T, url string, tune func(*Sink)) (*Sink, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	s := &Sink{
		Target:       func() string { return url },
		Secret:       []byte(testSecret),
		Login:        func(context.Context, int64) (string, error) { return "ivanov", nil },
		AllowPrivate: true,
		Log:          slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})),
		MaxAttempts:  3,
		BackoffBase:  time.Millisecond,
		BackoffCap:   5 * time.Millisecond,
		Workers:      1,
	}
	if tune != nil {
		tune(s)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return s, lb
}

func sampleEvent() Event {
	return Event{
		Kind: EventCompleted, SubID: 42, UserID: 7, TaskID: "01-intro",
		Status: "done", Raw: 80, Penalty: 10, Final: 72,
	}
}

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settle gives a wrong implementation time to make the request it must not make.
func settle() { time.Sleep(200 * time.Millisecond) }

// TestDeliversSignedPayload: one terminal submission produces exactly one
// delivery, the signature over "<timestamp>.<body>" verifies with the shared
// secret, and a body changed by one byte no longer does.
func TestDeliversSignedPayload(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	s, _ := newSink(t, rec.URL, nil)

	s.Send(sampleEvent())
	waitFor(t, "the delivery", func() bool { return rec.count() == 1 })
	settle()
	if n := rec.count(); n != 1 {
		t.Fatalf("receiver got %d requests, want exactly 1", n)
	}

	got := rec.all()[0]
	if v := got.headers.Get("X-Anygrade-Event"); v != EventCompleted {
		t.Errorf("X-Anygrade-Event = %q, want %q", v, EventCompleted)
	}
	if v := got.headers.Get("X-Anygrade-Attempt"); v != "1" {
		t.Errorf("X-Anygrade-Attempt = %q, want 1", v)
	}
	if v := got.headers.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q", v)
	}

	ts := got.headers.Get("X-Anygrade-Timestamp")
	if ts == "" {
		t.Fatal("no X-Anygrade-Timestamp: without it the signature can be replayed forever")
	}
	want := "v1=" + Sign([]byte(testSecret), ts, got.body)
	if sig := got.headers.Get("X-Anygrade-Signature"); sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
	// The timestamp is inside the signed material, so re-signing the same body
	// under a different one - the replay a captured delivery would attempt -
	// yields a signature the header no longer matches.
	if replay := "v1=" + Sign([]byte(testSecret), "1", got.body); replay == want {
		t.Error("the timestamp is not part of the signed material")
	}
	tampered := append(slices.Clone(got.body[:len(got.body)-1]), '}', ' ')
	if "v1="+Sign([]byte(testSecret), ts, tampered) == want {
		t.Error("a tampered body produced the same signature")
	}

	var p payload
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != EventCompleted || p.SubmissionID != 42 || p.Student != "ivanov" ||
		p.Task != "01-intro" || p.Status != "done" ||
		p.Score != 72 || p.RawScore != 80 || p.PenaltyPercent != 10 {
		t.Errorf("payload = %+v", p)
	}
	if p.SentAt == "" {
		t.Error("sent_at is empty")
	}
}

// TestPayloadCarriesOnlyIdentifiersAndScores pins the wire format field by
// field. Check excerpts, log paths and worker notes are teacher-only material
// (SPEC §14) and a webhook receiver is whatever the teacher pointed it at, so a
// field added here without thought has to fail a test rather than a course.
func TestPayloadCarriesOnlyIdentifiersAndScores(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	s, _ := newSink(t, rec.URL, nil)

	s.Send(sampleEvent())
	waitFor(t, "the delivery", func() bool { return rec.count() == 1 })

	var got map[string]any
	if err := json.Unmarshal(rec.all()[0].body, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"event", "penalty_percent", "raw_score", "score", "sent_at",
		"status", "student", "submission_id", "task"}
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
		t.Fatalf("payload keys = %v, want %v", keys, want)
	}
}

// TestNoConfigurationNoRequest: with nothing configured the feature is not
// merely quiet, it is absent - the server must behave exactly as it did before
// it existed.
func TestNoConfigurationNoRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		tune func(*Sink)
	}{
		{"no target", func(s *Sink) { s.Target = func() string { return "" } }},
		{"no target func", func(s *Sink) { s.Target = nil }},
		{"no secret", func(s *Sink) { s.Secret = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newReceiver(t, http.StatusOK)
			s, _ := newSink(t, rec.URL, tc.tune)
			s.Send(sampleEvent())
			settle()
			if n := rec.count(); n != 0 {
				t.Fatalf("receiver got %d requests, want 0", n)
			}
		})
	}
}

// TestRetriesToTheCapThenLogs: a receiver that is up but broken is retried up
// to the cap and then abandoned with a line in the log - the submission is
// already final in the database, so this is the only place the loss is visible.
func TestRetriesToTheCapThenLogs(t *testing.T) {
	rec := newReceiver(t, http.StatusInternalServerError)
	s, lb := newSink(t, rec.URL, nil)

	s.Send(sampleEvent())
	waitFor(t, "every attempt", func() bool { return rec.count() == s.MaxAttempts })
	settle()
	if n := rec.count(); n != s.MaxAttempts {
		t.Fatalf("receiver got %d requests, want the cap of %d", n, s.MaxAttempts)
	}
	for i, r := range rec.all() {
		if got, want := r.headers.Get("X-Anygrade-Attempt"), strconv.Itoa(i+1); got != want {
			t.Errorf("attempt header of request %d = %q, want %q", i, got, want)
		}
	}
	if !strings.Contains(lb.String(), "giving up") {
		t.Fatalf("no give-up line in the log:\n%s", lb.String())
	}
}

// TestPermanentRefusalIsNotRetried: a 4xx says the payload is wrong for that
// endpoint, so repeating it only burns the budget an outage would need.
func TestPermanentRefusalIsNotRetried(t *testing.T) {
	rec := newReceiver(t, http.StatusBadRequest)
	s, lb := newSink(t, rec.URL, nil)

	s.Send(sampleEvent())
	waitFor(t, "the delivery", func() bool { return rec.count() == 1 })
	settle()
	if n := rec.count(); n != 1 {
		t.Fatalf("receiver got %d requests, want 1", n)
	}
	if !strings.Contains(lb.String(), "not retrying") {
		t.Fatalf("no refusal line in the log:\n%s", lb.String())
	}
}

// TestSendNeverBlocks: Send is called from a queue worker immediately after the
// submission row was written, so a hung receiver may not slow it down, and a
// backlog may not grow without bound either. Both are the same guarantee: the
// queue overflows into the log, never into the caller.
func TestSendNeverBlocks(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	rec.block(t)
	s, lb := newSink(t, rec.URL, func(s *Sink) { s.QueueSize = 4 })
	s.Send(sampleEvent())
	waitFor(t, "the first delivery to hang", func() bool { return rec.count() == 1 })

	start := time.Now()
	for i := range 500 {
		ev := sampleEvent()
		ev.SubID = int64(i)
		s.Send(ev)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("500 sends took %s against a hung receiver; Send must not block", elapsed)
	}
	if !strings.Contains(lb.String(), "queue is full") {
		t.Fatalf("a dropped event must be logged:\n%s", lb.String())
	}
}

// TestShutdownAbandonsInFlightDelivery: a delivery in flight at shutdown must
// not hold the server open, and must not disappear without a line either.
func TestShutdownAbandonsInFlightDelivery(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	rec.block(t)
	lb := &logBuf{}
	s := &Sink{
		Target:       func() string { return rec.URL },
		Secret:       []byte(testSecret),
		Login:        func(context.Context, int64) (string, error) { return "ivanov", nil },
		AllowPrivate: true,
		Log:          slog.New(slog.NewTextHandler(lb, nil)),
		Timeout:      time.Minute, // long enough that only the cancel can end it
		QueueSize:    8,
		Workers:      1,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	s.Send(sampleEvent())
	waitFor(t, "the delivery to hang", func() bool { return rec.count() == 1 })
	// A second event is still queued behind the first: it never leaves.
	queued := sampleEvent()
	queued.SubID = 43
	s.Send(queued)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation: a pending delivery hung the shutdown")
	}
	log := lb.String()
	if !strings.Contains(log, "abandoned at shutdown") {
		t.Fatalf("the abandoned delivery was not reported:\n%s", log)
	}
	if !strings.Contains(log, "submission=43") {
		t.Fatalf("the still-queued event was dropped silently:\n%s", log)
	}
}

// TestSendAfterShutdownIsLogged: a queue worker may still be draining when the
// deliverer has already stopped. Its event cannot be delivered, and it must not
// end up in a channel nobody reads either - the loss has to be a line in the log.
func TestSendAfterShutdownIsLogged(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	lb := &logBuf{}
	s := &Sink{
		Target:       func() string { return rec.URL },
		Secret:       []byte(testSecret),
		Login:        func(context.Context, int64) (string, error) { return "ivanov", nil },
		AllowPrivate: true,
		Log:          slog.New(slog.NewTextHandler(lb, nil)),
		Workers:      1,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	cancel()
	<-done

	s.Send(sampleEvent())
	settle()
	if n := rec.count(); n != 0 {
		t.Fatalf("receiver got %d requests after shutdown, want 0", n)
	}
	if !strings.Contains(lb.String(), "dropped after shutdown") {
		t.Fatalf("the event vanished without a log line:\n%s", lb.String())
	}
}

// TestGiveUpLineWithholdsTheTarget: http.Client wraps every transport failure
// in an error that quotes the whole URL, and a webhook path is often the
// credential the receiver authenticates with. The log line names the cause,
// not the target.
func TestGiveUpLineWithholdsTheTarget(t *testing.T) {
	// Port 1 with nothing on it: the dial fails, so the error is the wrapped
	// transport one rather than a status code.
	const secretPath = "/hooks/tOkEnInThePath"
	s, lb := newSink(t, "http://127.0.0.1:1"+secretPath, nil)

	s.Send(sampleEvent())
	waitFor(t, "the deliverer to give up", func() bool {
		return strings.Contains(lb.String(), "giving up")
	})
	if strings.Contains(lb.String(), secretPath) {
		t.Fatalf("the log line quotes the target path:\n%s", lb.String())
	}
}

// TestPrivateTargetIsRefused enforces the address policy: the httptest receiver
// listens on 127.0.0.1, which is exactly what a course.yaml aimed at the
// server's own internals looks like. Nothing reaches it unless the operator
// allowed private targets.
func TestPrivateTargetIsRefused(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	s, lb := newSink(t, rec.URL, func(s *Sink) { s.AllowPrivate = false })

	s.Send(sampleEvent())
	waitFor(t, "the deliverer to give up", func() bool {
		return strings.Contains(lb.String(), "giving up")
	})
	if n := rec.count(); n != 0 {
		t.Fatalf("receiver on a loopback address got %d requests, want 0", n)
	}

	// The same target with the operator's switch on does connect, so the
	// refusal above is the policy and not a broken client.
	rec2 := newReceiver(t, http.StatusOK)
	s2, _ := newSink(t, rec2.URL, nil)
	s2.Send(sampleEvent())
	waitFor(t, "the allowed delivery", func() bool { return rec2.count() == 1 })
}

// TestRedirectIsNotFollowed: a redirect is how a target that passes the address
// policy hops to one that would not, so it is not followed at all - it is
// reported as the failed delivery it is.
func TestRedirectIsNotFollowed(t *testing.T) {
	final := newReceiver(t, http.StatusOK)
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	t.Cleanup(hop.Close)

	s, _ := newSink(t, hop.URL, nil)
	s.Send(sampleEvent())
	settle()
	if n := final.count(); n != 0 {
		t.Fatalf("the redirect target got %d requests, want 0", n)
	}
}
