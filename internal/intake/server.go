package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
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

// processPush is Intake.ProcessPush (SPEC §6): diff against the baseline,
// map changed paths to tasks, admit, enqueue, answer with `remote:` lines.
// Always exit 0: a student push is never failed by grading bookkeeping.
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
			lines = append(lines, s.gradePush(ctx, user, dir, u.New, now)...)
		}
	}
	if len(lines) == 0 {
		lines = []string{"anygrade: no tasks changed"}
	}
	return hookproto.Response{Lines: lines}
}

// gradePush handles one default-branch update.
func (s *Server) gradePush(ctx context.Context, user store.User, dir, newSHA string, now time.Time) []string {
	c := s.Course.Get()

	baseline := ""
	if out, err := gitserver.Git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/anygrade/baseline^{commit}"); err == nil {
		baseline = out
	}
	base := baseline
	if base == "" {
		base = emptyTree
	}
	diffOut, err := gitserver.Git(ctx, dir, "diff", "--name-only", base, newSHA)
	if err != nil && base != emptyTree {
		// Baseline object gone (gc after a force push): self-heal by
		// re-detecting everything instead of failing.
		baseline = ""
		diffOut, err = gitserver.Git(ctx, dir, "diff", "--name-only", emptyTree, newSHA)
	}
	if err != nil {
		return []string{"anygrade: change detection failed: " + err.Error()}
	}
	var paths []string
	for p := range strings.SplitSeq(diffOut, "\n") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	taskIDs, dropped := s.dropAlreadyRecorded(ctx, dir, user.ID, c, c.DetectTasks(paths), newSHA)

	// Explicit [recheck <task-id>] markers; scanned only against a real
	// baseline (a first push already detects every task by diff).
	if baseline != "" {
		if msgs, err := gitserver.Git(ctx, dir, "log", "--format=%B", baseline+".."+newSHA); err == nil {
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
		return append(lines, s.advanceBaseline(ctx, dir, newSHA, true)...)
	}

	width := 0
	for _, id := range taskIDs {
		width = max(width, len(id))
	}
	lines = append(lines, fmt.Sprintf("anygrade: %d task(s) detected", len(taskIDs)))
	processed := true
	for _, id := range taskIDs {
		task, _, _ := c.Task(id)
		sub, d, err := s.Queue.Submit(ctx, task, store.NewSubmission{
			UserID: user.ID, TaskID: id, CommitSHA: newSHA, ReceivedAt: now,
		}, false)
		if err != nil {
			lines = append(lines, fmt.Sprintf("  %-*s error: %v", width, id, err))
			processed = false
			continue
		}
		if !d.Admit {
			lines = append(lines, fmt.Sprintf("  %-*s rejected: %s", width, id, d.RejectReason))
			continue
		}
		line := fmt.Sprintf("  %-*s submission #%d queued", width, id, sub.ID)
		if s.BaseURL != "" {
			line += fmt.Sprintf("   %s/submissions/%d", strings.TrimSuffix(s.BaseURL, "/"), sub.ID)
		}
		lines = append(lines, line)
		if err := s.pinSubmission(ctx, dir, sub.ID, newSHA); err != nil {
			lines = append(lines, fmt.Sprintf("  %-*s warning: commit not pinned, a force push can drop it",
				width, id))
		}
	}
	return append(lines, s.advanceBaseline(ctx, dir, newSHA, processed)...)
}

// advanceBaseline moves refs/anygrade/baseline to newSHA, but only once every
// detected task has been processed. The ref is the "last processed commit"
// marker, so advancing it past a task that failed to record would lose that
// change for good: the next push would diff against a commit that already
// contains it. Deliberately not deferred, so the error paths above matter.
// Returns the push-output lines for the cases where the ref did not move.
func (s *Server) advanceBaseline(ctx context.Context, dir, newSHA string, processed bool) []string {
	if !processed {
		s.log().Warn("baseline kept after a processing error",
			"repo", dir, "commit", newSHA)
		return []string{"anygrade: baseline kept; the next push re-detects these changes"}
	}
	if _, err := gitserver.Git(ctx, dir, "update-ref", "refs/anygrade/baseline", newSHA); err != nil {
		s.log().Error("baseline update failed", "repo", dir, "commit", newSHA, "err", err)
		return []string{"anygrade: baseline update failed; the next push re-detects these changes"}
	}
	return nil
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
