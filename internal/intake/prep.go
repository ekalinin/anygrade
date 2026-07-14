package intake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/gitserver"
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
}

// Prepare implements queue.JobPrep. Errors are retryable infra failures,
// except an unknown task id, which is terminal (queue.ErrTaskGone).
func (p *Prep) Prepare(ctx context.Context, sub store.Submission) (queue.Prepared, error) {
	task, relDir, ok := p.Course.Get().Task(sub.TaskID)
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

	authoritative := gitserver.GitSource{Dir: p.Repos.CourseDir(), Commit: p.Course.Get().Head}
	student := gitserver.GitSource{Dir: studentDir, Commit: sub.CommitSHA}

	notes, err := gitserver.TamperNotes(ctx, authoritative, student, relDir, task.SolutionFiles)
	if err != nil {
		return queue.Prepared{}, fmt.Errorf("tamper scan: %w", err)
	}

	// Hidden tests: local source only in M4; the git-backed cache is M7.
	var hidden runner.Source
	if h := task.Hidden; h != nil && h.Source == "local" {
		if st, err := os.Stat(h.Path); err == nil && st.IsDir() {
			hidden = runner.WorkingCopySource{Root: h.Path}
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
			Hidden:        hidden,
			RunAsUID:      -1,
			RunAsGID:      -1,
		},
		Task:   task,
		LogDir: filepath.Join(p.DataDir, "logs", strconv.FormatInt(sub.ID, 10)),
		Note:   strings.Join(notes, "\n"),
	}, nil
}
