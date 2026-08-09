package intake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/hidden"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/store"
)

// Prep implements queue.JobPrep over the bare repos: authoritative files come
// from the course mirror at the loaded head, solution files from the
// student's submitted commit (SPEC §6.1).
type Prep struct {
	Repos   *gitserver.RepoManager
	Users   store.UserStore
	Course  *Holder
	DataDir string
	Hidden  *hidden.Cache // git-backed hidden tests; nil only in tests
}

// Prepare implements queue.JobPrep. Errors are retryable infra failures,
// except the teacher-config faults, which are terminal: an unknown task id
// (queue.ErrTaskGone) and hidden tests that cannot be resolved at all.
func (p *Prep) Prepare(ctx context.Context, sub store.Submission) (queue.Prepared, error) {
	// One snapshot for the whole preparation: the holder is swapped whole on a
	// teacher push, so reading it twice could pair one version's task metadata
	// with another version's authoritative tree.
	course := p.Course.Get()
	task, relDir, ok := course.Task(sub.TaskID)
	if !ok {
		return queue.Prepared{}, fmt.Errorf("%s: %w", sub.TaskID, queue.ErrTaskGone)
	}
	user, err := p.Users.GetUserByID(ctx, sub.UserID)
	if err != nil {
		return queue.Prepared{}, fmt.Errorf("resolve submission user: %w", err)
	}
	studentDir := p.Repos.StudentDir(user.Login)
	if _, err := os.Stat(studentDir); err != nil {
		return queue.Prepared{}, fmt.Errorf("student repo: %w", err)
	}

	authoritative := gitserver.GitSource{Dir: p.Repos.CourseDir(), Commit: course.Head}
	student := gitserver.GitSource{Dir: studentDir, Commit: sub.CommitSHA}

	notes, err := gitserver.TamperNotes(ctx, authoritative, student, relDir, task.SolutionFiles, task.Workspace.Include)
	if err != nil {
		return queue.Prepared{}, fmt.Errorf("tamper scan: %w", err)
	}

	var hiddenSrc runner.Source
	if h := task.Hidden; h != nil {
		switch h.Source {
		case "local":
			// A configured-but-unusable path must never be skipped: grading
			// the open tests only would silently hand out a passing score.
			// validate checks that path is set, not that it exists, so a
			// wrong path first shows up here - and retrying will not fix it,
			// exactly like the git source's ErrConfig below.
			if st, err := os.Stat(h.Path); err != nil || !st.IsDir() {
				return queue.Prepared{}, queue.Terminal("hidden tests unavailable for this task")
			}
			hiddenSrc = runner.WorkingCopySource{Root: h.Path}
		case "git":
			src, err := p.Hidden.Source(ctx, *h)
			if errors.Is(err, hidden.ErrConfig) {
				// Teacher config fault: retrying will not help.
				return queue.Prepared{}, queue.Terminal("hidden tests unavailable for this task")
			}
			if err != nil {
				return queue.Prepared{}, err // already scrubbed; retryable
			}
			hiddenSrc = src
		}
	}

	runID := fmt.Sprintf("sub-%d-%d", sub.ID, time.Now().UnixNano())
	return queue.Prepared{
		Assembly: runner.Assembly{
			Dest:          filepath.Join(p.DataDir, "workspaces", runID),
			Task:          task,
			TaskRelDir:    relDir,
			Include:       task.Workspace.Include,
			Authoritative: authoritative,
			Student:       student,
			Hidden:        hiddenSrc,
			RunAsUID:      -1,
			RunAsGID:      -1,
		},
		Task:   task,
		LogDir: SubmissionLogDir(p.DataDir, sub.ID),
		Note:   strings.Join(notes, "\n"),
	}, nil
}

// SubmissionLogDir is the single source of the per-submission log location:
// prep points the runner here, and the web layer tails it while the run is
// live (submissions.log_dir is only persisted at finish).
func SubmissionLogDir(dataDir string, subID int64) string {
	return filepath.Join(dataDir, "logs", strconv.FormatInt(subID, 10))
}
