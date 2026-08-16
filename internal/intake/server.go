package intake

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/hookproto"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/store"
)

// emptyTree is git's well-known empty tree: the diff base when a student repo
// has no baseline yet (first push) or the baseline object is gone (force-push
// self-healing, design risk #4).
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

const zeroSHA = "0000000000000000000000000000000000000000"

// recheckRe matches the explicit `[recheck <task-id>]` commit marker (SPEC §6).
var recheckRe = regexp.MustCompile(`\[recheck ([A-Za-z0-9._-]+)\]`)

// handleTimeout bounds one hook exchange so a stuck git call never wedges
// the socket (the hook client itself gives up after 30s).
const handleTimeout = 20 * time.Second

// Server is the unix-socket intake listener: the server counterpart of the
// `anygrade hook` client. It owns diff→Admit→enqueue and teacher-push
// validation; gitserver only ever sees the socket path.
type Server struct {
	DB      store.Store
	Queue   *queue.Queue
	Repos   *gitserver.RepoManager
	Course  *Holder
	BaseURL string       // submission link prefix in push output; "" = no links
	Log     *slog.Logger // server log for ref bookkeeping failures; nil = discard

	mu        sync.Mutex             // guards pushLocks
	pushLocks map[string]*sync.Mutex // per-student drain serialization
}

// pushLock lazily creates (and reuses) the mutex that serializes the push
// drains of one student.
func (s *Server) pushLock(login string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pushLocks == nil {
		s.pushLocks = make(map[string]*sync.Mutex)
	}
	lock, ok := s.pushLocks[login]
	if !ok {
		lock = &sync.Mutex{}
		s.pushLocks[login] = lock
	}
	return lock
}

// log returns the configured logger, or a discarding one so the zero value and
// tests work without wiring.
func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// ListenAndServe accepts hook connections on the unix socket until ctx is
// canceled. A stale socket file from a previous run is replaced.
func (s *Server) ListenAndServe(ctx context.Context, socket string) error {
	_ = os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			l.Close()
		case <-done:
		}
	}()
	defer close(done)

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(handleTimeout))
	hctx, cancel := context.WithTimeout(ctx, handleTimeout)
	defer cancel()

	var req hookproto.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	resp := s.dispatch(hctx, req)
	_ = json.NewEncoder(conn).Encode(resp)
}

func (s *Server) dispatch(ctx context.Context, req hookproto.Request) hookproto.Response {
	switch req.Kind {
	case hookproto.KindPreReceive:
		return preReceive(req)
	case hookproto.KindValidateCourse:
		return s.validateCourse(ctx, req)
	case hookproto.KindPostReceive:
		if req.Repo == "course" {
			return s.courseUpdated(ctx)
		}
		return s.processPush(ctx, req)
	default:
		return hookproto.Response{Lines: []string{"anygrade: unknown hook kind " + req.Kind}}
	}
}

// preReceive guards reserved refs in student repos. transfer.hideRefs already
// denies these; this is the belt to that suspender.
func preReceive(req hookproto.Request) hookproto.Response {
	for _, u := range req.Updates {
		if strings.HasPrefix(u.Ref, "refs/anygrade/") {
			return hookproto.Response{
				Lines:    []string{"anygrade: " + u.Ref + " is reserved"},
				ExitCode: 1,
			}
		}
	}
	return hookproto.Response{}
}

// validateCourse gates teacher pushes to the course repo: the pushed metadata
// must validate or the push is rejected with the error list (SPEC §7). The
// quarantine env from the hook lets us read not-yet-accepted objects.
func (s *Server) validateCourse(ctx context.Context, req hookproto.Request) hookproto.Response {
	courseDir := s.Repos.CourseDir()
	def, err := s.Repos.DefaultBranch(ctx, courseDir)
	if err != nil {
		return hookproto.Response{Lines: []string{"anygrade: " + err.Error()}, ExitCode: 1}
	}
	for _, u := range req.Updates {
		if strings.HasPrefix(u.Ref, "refs/anygrade/") {
			return hookproto.Response{Lines: []string{"anygrade: " + u.Ref + " is reserved"}, ExitCode: 1}
		}
		if u.Ref != "refs/heads/"+def {
			continue
		}
		if u.New == zeroSHA {
			return hookproto.Response{Lines: []string{"anygrade: refusing to delete the default branch"}, ExitCode: 1}
		}
		_, diags, err := LoadCourseAt(ctx, courseDir, u.New, quarantineEnv(req))
		if err != nil {
			return hookproto.Response{Lines: []string{"anygrade: validation failed: " + err.Error()}, ExitCode: 1}
		}
		if config.HasErrors(diags) {
			lines := []string{"anygrade: course metadata is invalid, push rejected:"}
			for _, d := range diags {
				if d.Severity == config.SevError {
					lines = append(lines, "  "+d.String())
				}
			}
			return hookproto.Response{Lines: lines, ExitCode: 1}
		}
	}
	return hookproto.Response{}
}

// courseUpdated reloads metadata after an accepted teacher push (validation
// already passed in pre-receive; a failure here only logs).
func (s *Server) courseUpdated(ctx context.Context) hookproto.Response {
	course, diags, err := LoadCourse(ctx, s.Repos.CourseDir())
	if err != nil || config.HasErrors(diags) {
		return hookproto.Response{Lines: []string{"anygrade: metadata reload failed; previous version stays active"}}
	}
	s.Course.Set(course)
	return hookproto.Response{Lines: []string{
		fmt.Sprintf("anygrade: course metadata reloaded (%d tasks)", len(course.Resolved.Tasks)),
	}}
}

// processPush is Intake.ProcessPush (SPEC §6): record each accepted update on
// the graded branch in the push log, then drain the student's pending pushes -
// diff, map changed paths to tasks, admit, enqueue - and answer with `remote:`
// lines. Always exit 0: a student push is never failed by grading bookkeeping.
func (s *Server) processPush(ctx context.Context, req hookproto.Request) hookproto.Response {
	now := time.Now()
	user, err := s.DB.GetUserByLogin(ctx, req.Repo)
	if err != nil {
		return hookproto.Response{Lines: []string{"anygrade: unknown repo owner " + req.Repo}}
	}
	dir := s.Repos.StudentDir(req.Repo)
	def, err := s.Repos.DefaultBranch(ctx, dir)
	if err != nil {
		return hookproto.Response{Lines: []string{"anygrade: " + err.Error()}}
	}

	var lines []string
	recorded := false
	for _, u := range req.Updates {
		switch {
		case u.Ref != "refs/heads/"+def:
			if strings.HasPrefix(u.Ref, "refs/heads/") && u.New != zeroSHA {
				lines = append(lines, fmt.Sprintf("anygrade: branch %s stored; only %s is graded",
					strings.TrimPrefix(u.Ref, "refs/heads/"), def))
			}
		case u.New == zeroSHA:
			lines = append(lines, "anygrade: default branch deleted; nothing to grade")
		default:
			// Recorded before anything is graded, and with the arrival time of
			// this exchange: everything downstream - deadlines, cooldown,
			// penalties - is evaluated at the moment the server accepted the
			// push, not whenever a handler got around to it.
			if _, err := s.DB.RecordPush(ctx, store.NewPush{UserID: user.ID, Ref: u.Ref,
				OldSHA: u.Old, NewSHA: u.New, ReceivedAt: now}); err != nil {
				s.log().Error("push not recorded", "repo", dir, "commit", u.New, "err", err)
				lines = append(lines, "anygrade: push not recorded: "+err.Error()+
					"; the next push re-detects these changes")
				continue
			}
			recorded = true
		}
	}
	if recorded {
		lines = append(lines, s.drainPushes(ctx, user, dir)...)
	}
	if len(lines) == 0 {
		lines = []string{"anygrade: no tasks changed"}
	}
	return hookproto.Response{Lines: lines}
}

// drainPushes grades every pending push of one student, oldest first.
//
// Hook connections are served concurrently and a handler can die between
// recording a push and grading it, so the handler that gets here is not
// necessarily grading its own push - it grades whatever is still pending,
// which is what keeps a lost handler from stalling the student's queue.
//
// Serializing the drains of one student is what makes "oldest first" mean
// anything: two overlapping drains would read the same pending set and grade
// every row in it twice. The wait is bounded by handleTimeout, which every
// exchange carries.
func (s *Server) drainPushes(ctx context.Context, user store.User, dir string) []string {
	lock := s.pushLock(user.Login)
	lock.Lock()
	defer lock.Unlock()

	pending, err := s.DB.PendingPushes(ctx, user.ID)
	if err != nil {
		s.log().Error("push log unavailable", "repo", dir, "err", err)
		return []string{"anygrade: push log unavailable: " + err.Error()}
	}
	if len(pending) == 0 {
		// Another handler drained this student while we waited for the lock;
		// it printed the outcome to its own push.
		return nil
	}

	// refs/anygrade/baseline is no longer where a push's range comes from - the
	// row carries its own ends. What it still is, is the high-water mark of
	// graded content, and that covers the one gap the log cannot: a push whose
	// hook never reached the server has no row at all. The oldest pending push
	// gets it as its recovery scan (gradePush).
	baseline := ""
	if out, err := gitserver.Git(ctx, dir, "rev-parse", "--verify", "--quiet",
		"refs/anygrade/baseline"); err == nil {
		baseline = out
	}

	var lines []string
	if len(pending) > 1 {
		lines = append(lines, fmt.Sprintf("anygrade: %d push(es) to grade, oldest first", len(pending)))
	}
	var graded []string
	for i, p := range pending {
		if ctx.Err() != nil {
			lines = append(lines, "anygrade: out of time; the rest is graded on the next push")
			break
		}
		mark := ""
		if i == 0 {
			mark = baseline
		}
		out, processed := s.gradePush(ctx, user, dir, p, mark)
		lines = append(lines, out...)
		if !processed {
			lines = append(lines, "anygrade: push kept in the log; the next push grades it again")
			break
		}
		if err := s.DB.MarkPushProcessed(ctx, p.ID, time.Now()); err != nil {
			s.log().Error("push not marked processed", "push", p.ID, "repo", dir, "err", err)
			lines = append(lines, "anygrade: push kept in the log; the next push grades it again")
			break
		}
		graded = append(graded, p.NewSHA)
	}
	// The mark moves only when this drain graded the commit the branch now
	// points at. Rows are drained in id order, which is the order the handlers
	// reached the store - close to the order the pushes arrived, but not it, so
	// the last row drained is not always the newest commit. The branch tip is
	// the one thing that is authoritative, because receive-pack moves it under
	// its own lock. When it is not among the commits just graded, either a push
	// we have no row for yet is in flight, or an older push was drained after a
	// newer one; in both cases the mark stays where it is. It may only ever
	// mean "everything up to here has been graded".
	if head, err := gitserver.Git(ctx, dir, "rev-parse", "--verify", "--quiet",
		pending[0].Ref); err == nil && slices.Contains(graded, head) {
		lines = append(lines, s.advanceBaseline(ctx, dir, baseline, head)...)
	}
	return lines
}

// gradePush grades one recorded push over its own range: from the tip it
// replaced to the tip it set. mark is refs/anygrade/baseline when this is the
// oldest pending push and "" otherwise - see the recovery scan below.
//
// The second return value reports whether the push can be marked processed:
// false only when a detected task failed to record, so that the push stays in
// the log and is graded again rather than lost.
func (s *Server) gradePush(ctx context.Context, user store.User, dir string,
	p store.Push, mark string) ([]string, bool) {

	c := s.Course.Get()

	from := p.OldSHA
	if from == zeroSHA {
		from = emptyTree
	}
	paths, err := changedPaths(ctx, dir, from, p.NewSHA)
	if err != nil && from != emptyTree {
		// The replaced tip is gone (gc after a force push): self-heal by
		// re-detecting everything instead of failing. The empty tree already
		// covers whatever the recovery scan below would have added.
		from = emptyTree
		paths, err = changedPaths(ctx, dir, emptyTree, p.NewSHA)
	}
	if err != nil {
		// Even the empty tree failed, so the pushed commit itself is
		// unreadable and no later handler can do better. Marking the push
		// processed anyway keeps it from blocking every push behind it.
		s.log().Error("change detection failed",
			"push", p.ID, "repo", dir, "commit", p.NewSHA, "err", err)
		return []string{"anygrade: change detection failed: " + err.Error()}, true
	}

	// Recovery scan. A push whose hook never reached the server leaves no row,
	// and its content would sit in the gap between the graded mark and the tip
	// this push replaced - a gap no row covers. It is folded in here because
	// this is the first handler that can see it, and only here: merging it into
	// the range above instead would let a push and its revert cancel out, and
	// then neither of the two pushes that really happened has any content left
	// to grade. Errors are not fatal - the mark can point at a gc'd commit -
	// and whatever it re-detects, the filter below drops if it was graded
	// already.
	if from != emptyTree && mark != "" && mark != p.OldSHA {
		gap, err := changedPaths(ctx, dir, mark, p.OldSHA)
		if err != nil {
			s.log().Warn("recovery scan skipped",
				"push", p.ID, "repo", dir, "from", mark, "err", err)
		}
		paths = append(paths, gap...)
	}

	taskIDs, dropped := s.dropAlreadyRecorded(ctx, dir, user.ID, c, c.DetectTasks(paths), p.NewSHA)

	// Explicit [recheck <task-id>] markers, scanned over the commits this push
	// introduced and nothing else. The content diff above may reach further
	// back than that when the baseline lags, and re-reading a range is harmless
	// there - the re-detection filter drops what was already graded at the same
	// content. A marker is the one thing that filter lets through by design, so
	// a range walked twice would charge two attempts for one marker. The cost
	// is that a marker in a push whose hook never reached the server is lost;
	// its content changes are not.
	if p.OldSHA != zeroSHA {
		if msgs, err := gitserver.Git(ctx, dir, "log", "--format=%B", p.OldSHA+".."+p.NewSHA); err == nil {
			for _, m := range recheckRe.FindAllStringSubmatch(msgs, -1) {
				if _, _, ok := c.Task(m[1]); ok && !slices.Contains(taskIDs, m[1]) {
					taskIDs = append(taskIDs, m[1])
				}
			}
		}
	}

	// A marker can bring a dropped task back; then it was not skipped.
	skipped := 0
	for _, id := range dropped {
		if !slices.Contains(taskIDs, id) {
			skipped++
		}
	}
	var lines []string
	if skipped > 0 {
		lines = append(lines, fmt.Sprintf(
			"anygrade: %d task(s) already graded at this content, skipped", skipped))
	}

	if len(taskIDs) == 0 {
		if skipped == 0 {
			lines = append(lines, "anygrade: no tasks changed")
		}
		return lines, true
	}

	width := 0
	for _, id := range taskIDs {
		width = max(width, len(id))
	}
	lines = append(lines, fmt.Sprintf("anygrade: %d task(s) detected", len(taskIDs)))
	processed := true
	for _, id := range taskIDs {
		task, _, _ := c.Task(id)
		// ReceivedAt is the push's own arrival time, not this handler's clock:
		// a push graded late - because its handler died, or because it queued
		// behind an older one - must still be judged against the deadline,
		// cooldown and penalty of the moment it arrived.
		sub, d, err := s.Queue.Submit(ctx, task, store.NewSubmission{
			UserID: user.ID, TaskID: id, CommitSHA: p.NewSHA, ReceivedAt: p.ReceivedAt,
		}, false)
		if err != nil {
			lines = append(lines, fmt.Sprintf("  %-*s error: %v", width, id, err))
			processed = false
			continue
		}
		// Pinning happens before the admission branch: a rejected submission
		// is a recorded row like any other, its page links to the submitted
		// code, and this ref is the only thing that keeps that tree alive once
		// a force push moves the branch and gc runs (SPEC §6 step 7).
		pinErr := s.pinSubmission(ctx, dir, sub.ID, p.NewSHA)
		if d.Admit {
			line := fmt.Sprintf("  %-*s submission #%d queued", width, id, sub.ID)
			if s.BaseURL != "" {
				line += fmt.Sprintf("   %s/submissions/%d", strings.TrimSuffix(s.BaseURL, "/"), sub.ID)
			}
			lines = append(lines, line)
		} else {
			lines = append(lines, fmt.Sprintf("  %-*s rejected: %s", width, id, d.RejectReason))
		}
		if pinErr != nil {
			lines = append(lines, fmt.Sprintf("  %-*s warning: commit not pinned, a force push can drop it",
				width, id))
		}
	}
	return lines, processed
}

// changedPaths lists the repository paths whose content differs between two
// trees. Ancestry is irrelevant: after a force push the topology lies and the
// content does not.
func changedPaths(ctx context.Context, dir, from, to string) ([]string, error) {
	out, err := gitserver.Git(ctx, dir, "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range strings.SplitSeq(out, "\n") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// advanceBaseline moves refs/anygrade/baseline to newSHA, the newest commit
// the drain graded. The ref is the high-water mark of graded content: the push
// log now owns the per-push ranges, but a push whose hook never reached the
// server leaves no row at all, and only this mark can tell the next drain how
// far back to look. Advancing it past a push that failed to record would lose
// that push's changes twice over, so the caller advances it to the last push it
// actually graded, and no further. Returns the push-output lines for the cases
// where the ref did not move.
//
// The move is a compare-and-swap against oldSHA, the value read at the start of
// the drain. Within this process the per-student lock already rules out a
// competing writer; the guard is what covers a ref moved from outside it, where
// a blind write would roll the mark back. An empty oldSHA means the ref must
// still be absent, which git spells as the zero SHA - the legacy repos without
// a baseline are exactly the ones re-detecting everything, so they need the
// guard most.
func (s *Server) advanceBaseline(ctx context.Context, dir, oldSHA, newSHA string) []string {
	expected := cmp.Or(oldSHA, zeroSHA)
	if _, err := gitserver.Git(ctx, dir, "update-ref", "refs/anygrade/baseline", newSHA, expected); err != nil {
		if staleBaseline(err) {
			// Unreachable while the handlers of one student are serialized;
			// kept as a backstop for a ref moved from outside this process,
			// where the mutex does not reach. Whoever moved it walked these
			// changes too, so the student has nothing to act on: log only.
			s.log().Info("baseline left to a concurrent writer",
				"repo", dir, "commit", newSHA, "expected", expected)
			return nil
		}
		s.log().Error("baseline update failed", "repo", dir, "commit", newSHA, "err", err)
		return []string{"anygrade: baseline update failed; the next push re-detects these changes"}
	}
	return nil
}

// staleBaselineReasons are the tails git appends to "cannot lock ref <ref>"
// when update-ref's old value does not match, observed on git 2.55:
//
//	is at <sha> but expected <sha>       the ref moved under us
//	reference already exists             we expected it to be absent
//	unable to resolve reference '<ref>'  we expected it to be present
//
// The exit status cannot tell these apart: every update-ref failure exits 128,
// a genuinely broken ref store included (a D/F conflict reports "<ref> exists;
// cannot create <ref>/<x>"). So the reason has to come from the message, and
// the match is deliberately narrow: an unrecognised failure is reported as an
// error, which is the pre-existing behaviour.
var staleBaselineReasons = []string{
	"but expected ",
	"reference already exists",
	"unable to resolve reference",
}

// staleBaseline reports whether err is update-ref rejecting our expected old
// value rather than failing to write the ref at all.
func staleBaseline(err error) bool {
	msg := err.Error()
	if !strings.Contains(msg, "cannot lock ref") {
		return false
	}
	// A ref file that does not hold a resolvable object id reports "unable to
	// resolve reference '<ref>': reference broken". It shares the tail a
	// mismatch produces but is not one: git refuses to move such a ref with
	// any expected value, and refuses to delete it as well, so the repo cannot
	// recover on its own. Reporting it is the only thing that can.
	if strings.Contains(msg, "reference broken") {
		return false
	}
	return slices.ContainsFunc(staleBaselineReasons, func(r string) bool {
		return strings.Contains(msg, r)
	})
}

// pinSubmission creates refs/anygrade/submissions/<id> at commit so a graded
// tree survives a force push (SPEC §6 step 7). The submission is already
// queued by the time this runs, so a failure is reported and logged, never
// fatal: grading proceeds, only the audit guarantee is lost. Details stay in
// the server log; the student-visible line says what it means for them.
func (s *Server) pinSubmission(ctx context.Context, dir string, subID int64, commit string) error {
	_, err := gitserver.Git(ctx, dir, "update-ref",
		fmt.Sprintf("refs/anygrade/submissions/%d", subID), commit)
	if err != nil {
		s.log().Error("pinning the submitted commit failed",
			"submission", subID, "repo", dir, "commit", commit, "err", err)
	}
	return err
}

// dropAlreadyRecorded splits detected tasks into the ones still to grade and
// the ones whose content has not moved since the student's own last
// submission for them.
//
// refs/anygrade/baseline marks the last processed commit, but it can lag
// behind what is already recorded: kept in place after a task fails to
// record, rolled back by a concurrent hook, or missing entirely on a repo
// provisioned before the baseline seed - where the diff falls back to the
// empty tree and re-detects every task that has files. Each re-detection
// would write another counting submission and charge another attempt for work
// that was already graded. The commit of the last recorded submission is the
// same marker at per-task resolution, and it does not lag.
//
// Content is compared over the task directory, the criterion DetectTasks uses
// (course.go), so a task is dropped by exactly what detected it. Ancestry is
// deliberately not consulted: after a force push the topology lies and the
// content does not.
//
// Any failure keeps the task and is logged: grading it again is the behaviour
// that existed before this filter, so a spare attempt is the worst outcome,
// while dropping a task on a failed check would lose the submission entirely.
func (s *Server) dropAlreadyRecorded(ctx context.Context, dir string, userID int64,
	c *Course, taskIDs []string, newSHA string) (keep, dropped []string) {

	for _, id := range taskIDs {
		_, relDir, ok := c.Task(id)
		if !ok {
			keep = append(keep, id)
			continue
		}
		last, found, err := s.DB.LastByUserTask(ctx, userID, id)
		if err != nil {
			s.log().Warn("re-detection check skipped: history unavailable",
				"task", id, "user", userID, "err", err)
		}
		if err != nil || !found {
			keep = append(keep, id)
			continue
		}
		changed, err := gitserver.Git(ctx, dir, "diff", "--name-only",
			last.CommitSHA, newSHA, "--", relDir)
		if err != nil {
			// Typically the recorded commit is unreachable: a force push
			// before its pin ref landed.
			s.log().Warn("re-detection check skipped: diff failed",
				"task", id, "repo", dir, "from", last.CommitSHA, "err", err)
		}
		if err != nil || changed != "" {
			keep = append(keep, id)
			continue
		}
		dropped = append(dropped, id)
	}
	return keep, dropped
}

func quarantineEnv(req hookproto.Request) []string {
	var env []string
	if req.ObjectDir != "" {
		env = append(env, "GIT_OBJECT_DIRECTORY="+req.ObjectDir)
	}
	if req.AltObjectDirs != "" {
		env = append(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+req.AltObjectDirs)
	}
	if req.QuarantinePath != "" {
		env = append(env, "GIT_QUARANTINE_PATH="+req.QuarantinePath)
	}
	return env
}
