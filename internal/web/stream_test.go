package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/store"
)

// sseProbe is a streaming ResponseWriter that ends the request as soon as the
// stream has written what the test was waiting for. SSE handlers block until
// the client goes away, so a recorder alone would hang, and a timer would race.
type sseProbe struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	hdr    http.Header
	marker string
	once   sync.Once
	seen   chan struct{}
}

func newSSEProbe(marker string) *sseProbe {
	return &sseProbe{hdr: http.Header{}, marker: marker, seen: make(chan struct{})}
}

func (p *sseProbe) Header() http.Header { return p.hdr }
func (p *sseProbe) WriteHeader(int)     {}
func (p *sseProbe) Flush()              {}

func (p *sseProbe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, err := p.buf.Write(b)
	if strings.Contains(p.buf.String(), p.marker) {
		p.once.Do(func() { close(p.seen) })
	}
	return n, err
}

func (p *sseProbe) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// stream drives one SSE route and returns everything it wrote, ending the
// request once the probe's marker shows up (or after a backstop timeout, so a
// regression fails instead of hanging the suite).
func stream(t *testing.T, h *Handler, target, marker string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := newSSEProbe(marker)
	stop := time.AfterFunc(5*time.Second, cancel)
	defer stop.Stop()
	go func() {
		<-p.seen
		cancel()
	}()
	New(h).ServeHTTP(p, httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx))
	return p.String()
}

// TestSSESendFramesCarriageReturns: the event-stream format ends a line on CR
// as well as LF, so a lone \r in check output used to close the `data:` line
// early and let the rest be read as stream syntax - one carriage return in a
// student's test output injected events into the teacher's stream.
func TestSSESendFramesCarriageReturns(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, ok := newSSEWriter(rec)
	if !ok {
		t.Fatal("recorder is not a Flusher")
	}
	sse.send("log0", "progress\rerror: boom\nevent: done\r\ndata: \r\n")

	out := rec.Body.String()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "data: ") {
			continue
		}
		if line != "event: log0" {
			t.Errorf("payload framed as stream syntax: %q\nfull stream:\n%s", line, out)
		}
	}
	if strings.ContainsRune(out, '\r') {
		t.Errorf("a raw carriage return survived into the stream:\n%q", out)
	}
}

// TestSSESendStripsBreaksFromEventName: an event name has no multi-line form,
// so a break in one (a task id is a directory name) could only be framing.
func TestSSESendStripsBreaksFromEventName(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, _ := newSSEWriter(rec)
	sse.send("task-t1\rdata: x", "ok")
	if got := rec.Body.String(); !strings.HasPrefix(got, "event: task-t1data: x\n") {
		t.Errorf("event name not sanitized: %q", got)
	}
}

// TestTerminalSubmission pins the one status that cannot be decided by name.
// The queue model makes an infra_error terminal when no retry is scheduled or
// a teacher canceled it; treating those as running left the page promising a
// "retrying" that never came and the stream open until the client gave up.
func TestTerminalSubmission(t *testing.T) {
	soon := time.Now().Add(time.Minute)
	now := time.Now()
	for _, tc := range []struct {
		name string
		sub  store.Submission
		want bool
	}{
		{name: "queued", sub: store.Submission{Status: store.StatusQueued}},
		{name: "running", sub: store.Submission{Status: store.StatusRunning}},
		{name: "retrying", sub: store.Submission{Status: store.StatusInfraError, RetryAt: &soon}},
		{name: "retries exhausted", sub: store.Submission{Status: store.StatusInfraError}, want: true},
		{name: "canceled", sub: store.Submission{Status: store.StatusInfraError, RetryAt: &soon, CanceledAt: &now}, want: true},
		{name: "done", sub: store.Submission{Status: store.StatusDone}, want: true},
		{name: "rejected", sub: store.Submission{Status: store.StatusRejectedDeadline}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalSubmission(tc.sub); got != tc.want {
				t.Errorf("terminalSubmission = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSubmissionStreamClosesOnExhaustedRetries: retries are spent, so the
// stream must say `done` and hang up rather than stay open forever.
func TestSubmissionStreamClosesOnExhaustedRetries(t *testing.T) {
	h, teacher := newTestSite(t)
	h.DataDir = t.TempDir()
	_, sub := erroredRow(t, h, "bob") // ScheduleRetry with a nil retryAt
	h.Local = &teacher

	got := stream(t, h, "/submissions/"+itoa(sub.ID)+"/stream", "event: done")
	if !strings.Contains(got, "event: done") {
		t.Fatalf("terminal infra_error did not end the stream:\n%q", got)
	}
}

// TestSubmissionPageStopsRetryingOnTerminalError: the same row must not render
// as live either - no SSE connection, and the badge says `error`, not
// `retrying`.
func TestSubmissionPageStopsRetryingOnTerminalError(t *testing.T) {
	h, teacher := newTestSite(t)
	_, sub := erroredRow(t, h, "bob")
	h.Local = &teacher

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/submissions/"+itoa(sub.ID), nil))
	body := rec.Body.String()
	if strings.Contains(body, "sse-connect") {
		t.Errorf("a terminal submission still opens a live stream:\n%s", body)
	}
	if !strings.Contains(body, gradebook.StatusError) {
		t.Errorf("badge does not show the terminal error status:\n%s", body)
	}
}

// TestQueueStreamReconcilesRenderedRows: the page renders its snapshot before
// the EventSource exists, and the Hub may drop on overflow, so a terminal flip
// in that gap is simply lost. The stream re-renders the rows the page put in
// the DOM as soon as the subscription is live.
func TestQueueStreamReconcilesRenderedRows(t *testing.T) {
	h, teacher := newTestSite(t)
	h.Local = &teacher
	student, err := h.DB.CreateUser(t.Context(), "bob", "Student", "student")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := h.DB.Enqueue(t.Context(), store.NewSubmission{
		UserID: student.ID, TaskID: "t1", CommitSHA: "deadbeef",
		ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The page rendered this row as queued; it finished before the stream
	// attached, and nobody was subscribed to hear about it.
	if _, ok, err := h.DB.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := h.DB.FinishSubmission(t.Context(), sub.ID, store.SubmissionResult{
		Status: store.StatusDone,
	}); err != nil {
		t.Fatal(err)
	}

	marker := "event: sub-" + itoa(sub.ID)
	got := stream(t, h, "/queue/stream?ids="+itoa(sub.ID), marker)
	if !strings.Contains(got, marker) {
		t.Fatalf("the stream did not reconcile the rendered row:\n%q", got)
	}
	if !strings.Contains(got, store.StatusDone) {
		t.Errorf("reconciled row does not carry the final status:\n%q", got)
	}
}

// TestDashboardStreamReconcilesEveryRow: same gap, and the dashboard needs no
// hint from the client - the row set is the course task list.
func TestDashboardStreamReconcilesEveryRow(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	student, err := h.DB.CreateUser(t.Context(), "bob", "Student", "student")
	if err != nil {
		t.Fatal(err)
	}
	h.Local = &student

	got := stream(t, h, "/dashboard/stream", "event: task-t2")
	for _, want := range []string{"event: task-t1", "event: task-t2"} {
		if !strings.Contains(got, want) {
			t.Errorf("no post-subscribe snapshot for %q:\n%q", want, got)
		}
	}
}

// TestMatrixStreamReconcilesEveryRow: the teacher board reconciles the whole
// snapshot it just rendered, for the same reason.
func TestMatrixStreamReconcilesEveryRow(t *testing.T) {
	h, teacher := newTestSite(t)
	setCourse(h)
	h.Local = &teacher
	if _, err := h.DB.CreateUser(t.Context(), "bob", "Student", "student"); err != nil {
		t.Fatal(err)
	}

	got := stream(t, h, "/matrix/stream", "event: user-bob")
	if !strings.Contains(got, "event: user-bob") {
		t.Fatalf("the matrix stream sent no post-subscribe snapshot:\n%q", got)
	}
}

// TestSubmissionStreamNeverTailsTheBuildLog: the live view exists so a student
// can watch their own tests run. The build phase is the one that compiles
// against the hidden tests, so its log must not be in the set of files the
// stream follows - it lives in its own subdirectory of the log dir, and the
// stream only ever opens LogFileName(check) directly under that dir (SPEC §14).
func TestSubmissionStreamNeverTailsTheBuildLog(t *testing.T) {
	const secret = "hidden_test.go:7: undefined: Solve"
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	holder := &intake.Holder{}
	holder.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{Name: "Test course"},
		Tasks: []config.ResolvedTask{{
			ID:     "t1",
			Checks: []config.Check{{Name: "compiled", Weight: 1, Build: "build it", Run: "run it"}},
		}},
	}})
	h.Course = holder

	student, err := h.DB.CreateUser(t.Context(), "bob", "Student", "student")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := h.DB.Enqueue(t.Context(), store.NewSubmission{
		UserID: student.ID, TaskID: "t1", CommitSHA: "deadbeef",
		ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.DB.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	dir := intake.SubmissionLogDir(h.DataDir, sub.ID)
	buildDir := runner.BuildLogDir(dir)
	if err := os.MkdirAll(buildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, runner.LogFileName("compiled")), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runner.LogFileName("compiled")), []byte("student visible output"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.Local = &student
	got := stream(t, h, "/submissions/"+itoa(sub.ID)+"/stream", "student visible output")
	if strings.Contains(got, secret) {
		t.Fatalf("the live stream tailed the build phase log:\n%q", got)
	}
	if !strings.Contains(got, "student visible output") {
		t.Fatalf("the live stream lost the run phase log:\n%q", got)
	}
}
