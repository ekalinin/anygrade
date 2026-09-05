package queue

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/store"
	"github.com/ekalinin/anygrade/internal/webhook"
)

// recorder is a Notifier that only remembers, so the completion contract can be
// checked without anything HTTP-shaped in the way.
type recorder struct {
	mu   sync.Mutex
	seen []Completion
}

func (r *recorder) Completed(c Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, c)
}

func (r *recorder) all() []Completion {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Completion(nil), r.seen...)
}

// TestCompletionFiresOnceWithScores: a graded submission produces exactly one
// completion, carrying the scores that were written to the row. A submission
// the policy rejected produces none - it never entered the queue, so nothing is
// waiting for its outcome.
func TestCompletionFiresOnceWithScores(t *testing.T) {
	q, db, u, _ := newTestQueue(t)
	rec := &recorder{}
	q.Notify = rec

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, db, sub.ID, store.StatusDone)

	// A rejection is recorded for the student's history but never queued, so it
	// must not look like a completed check to an external system.
	hard := time.Now().Add(-time.Hour)
	if _, d, err := q.Submit(ctx, policyTask(0, 0, &hard), store.NewSubmission{
		UserID: u.ID, TaskID: "late", CommitSHA: "def", ReceivedAt: time.Now(),
	}, false); err != nil || d.Admit {
		t.Fatalf("Submit: admit=%v err=%v", d.Admit, err)
	}
	// Give a wrong implementation time to fire the extra event.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("got %d completions, want exactly 1: %+v", len(got), got)
	}
	want := Completion{
		SubID: sub.ID, UserID: u.ID, TaskID: "t1", Status: store.StatusDone,
		Raw: 60, Penalty: 20, Final: 48,
	}
	if got[0] != want {
		t.Fatalf("completion = %+v, want %+v", got[0], want)
	}
}

// sinkNotifier is the adapter internal/app owns in production. The tests below
// need it here because the property they check spans both packages: queue stays
// free of HTTP, and that is precisely what has to hold when the HTTP end is
// broken.
type sinkNotifier struct{ sink *webhook.Sink }

func (n sinkNotifier) Completed(c Completion) {
	n.sink.Send(webhook.Event{
		Kind: webhook.EventCompleted, SubID: c.SubID, UserID: c.UserID,
		TaskID: c.TaskID, Status: c.Status,
		Raw: c.Raw, Penalty: c.Penalty, Final: c.Final,
	})
}

// syncBuf collects slog output written from several delivery goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// withSink wires a real deliverer into the queue and runs it, returning its log
// and a stop function that waits for it. AllowPrivate is on because the test
// receiver listens on 127.0.0.1.
func withSink(t *testing.T, q *Queue, db *store.DB, url string, tune func(*webhook.Sink)) (*syncBuf, func()) {
	t.Helper()
	lb := &syncBuf{}
	sink := &webhook.Sink{
		Target: func() string { return url },
		Secret: []byte("topsecret"),
		Login: func(ctx context.Context, id int64) (string, error) {
			u, err := db.GetUserByID(ctx, id)
			return u.Login, err
		},
		AllowPrivate: true,
		Log:          slog.New(slog.NewTextHandler(lb, nil)),
		MaxAttempts:  2,
		BackoffBase:  time.Millisecond,
		Workers:      1,
		QueueSize:    8,
	}
	if tune != nil {
		tune(sink)
	}
	q.Notify = sinkNotifier{sink}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sink.Run(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the deliverer did not stop: a pending delivery hung the shutdown")
		}
	}
	t.Cleanup(stop)
	return lb, stop
}

// gradeTwo enqueues two submissions on different tasks and waits for both to be
// graded. Different tasks on purpose: submissions of one (student, task) pair
// run in order by design, and what is under test here is the deliverer.
func gradeTwo(t *testing.T, q *Queue, db *store.DB, u store.User) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()
	defer func() { cancel(); <-done }()

	for _, task := range []string{"t1", "t2"} {
		sub, err := q.Enqueue(ctx, store.NewSubmission{
			UserID: u.ID, TaskID: task, CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		waitStatus(t, db, sub.ID, store.StatusDone)
	}
}

// TestGradingIgnoresABrokenReceiver is the property this whole feature is
// judged on: a receiver answering 500 changes nothing about the submissions.
// They are graded, they are done, and the delivery gives up on its own with a
// line in the log.
func TestGradingIgnoresABrokenReceiver(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	q, db, u, _ := newTestQueue(t)
	lb, stop := withSink(t, q, db, srv.URL, nil)
	gradeTwo(t, q, db, u)

	// Both submissions are already done; the deliveries are still failing
	// behind them, which is the whole point.
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(lb.String(), "giving up") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if got := strings.Count(lb.String(), "giving up"); got != 2 {
		t.Fatalf("%d give-up lines, want 2:\n%s", got, lb.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 4 { // two events, two attempts each
		t.Errorf("receiver was hit %d times, want 4 (2 events x MaxAttempts)", hits)
	}
}

// TestGradingIgnoresAHungReceiver: the same, for the harder case. Every
// delivery is stuck inside the request, so the whole delivery path is occupied
// while the workers grade - and the shutdown still terminates, saying what it
// dropped.
func TestGradingIgnoresAHungReceiver(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	q, db, u, _ := newTestQueue(t)
	// A minute, so only the shutdown can end an attempt.
	lb, stop := withSink(t, q, db, srv.URL, func(s *webhook.Sink) { s.Timeout = time.Minute })
	gradeTwo(t, q, db, u)
	stop()

	if !strings.Contains(lb.String(), "abandoned at shutdown") {
		t.Fatalf("the abandoned delivery was not reported:\n%s", lb.String())
	}
}
