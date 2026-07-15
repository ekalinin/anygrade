package intake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/store"
)

// ErrNothingToRecheck marks a recheck request for a task the student never
// submitted; the UI disables the button in that state.
var ErrNothingToRecheck = errors.New("no graded commit to recheck")

// Recheck re-grades the latest counting submission's commit (SPEC §6: the UI
// recheck button; student-initiated, so it goes through Admit and counts
// against max_attempts and cooldown). Living here keeps git out of web.
func (s *Server) Recheck(ctx context.Context, userID int64, taskID string) (store.Submission, queue.Decision, error) {
	task, _, ok := s.Course.Get().Task(taskID)
	if !ok {
		return store.Submission{}, queue.Decision{}, fmt.Errorf("unknown task %q", taskID)
	}
	user, err := s.DB.GetUserByID(ctx, userID)
	if err != nil {
		return store.Submission{}, queue.Decision{}, err
	}
	history, err := s.DB.ListByUserTask(ctx, userID, taskID)
	if err != nil {
		return store.Submission{}, queue.Decision{}, err
	}
	commit := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Counts {
			commit = history[i].CommitSHA
			break
		}
	}
	if commit == "" {
		return store.Submission{}, queue.Decision{}, ErrNothingToRecheck
	}

	now := time.Now()
	d := queue.Admit(task, history, now, false)
	ns := store.NewSubmission{
		UserID: userID, TaskID: taskID, CommitSHA: commit,
		ReceivedAt: now, Counts: d.Counts,
	}
	if !d.Admit {
		sub, err := s.DB.RecordRejected(ctx, ns, d.RejectStatus)
		if err != nil {
			return store.Submission{}, d, err
		}
		s.publish(sub, d.RejectStatus)
		return sub, d, nil
	}
	sub, err := s.Queue.Enqueue(ctx, ns)
	if err != nil {
		return store.Submission{}, d, err
	}
	// Pin the rechecked commit like a push does (SPEC §6 step 7).
	_, _ = gitserver.Git(ctx, s.Repos.StudentDir(user.Login), "update-ref",
		fmt.Sprintf("refs/anygrade/submissions/%d", sub.ID), commit)
	return sub, d, nil
}
