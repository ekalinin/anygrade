package intake

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	return s.recheck(ctx, userID, taskID, false)
}

// TeacherRecheck re-grades a student's latest counting commit on a teacher's
// behalf: Admit(teacherRecheck=true) bypasses limits and deadlines, and the
// submission never consumes an attempt (SPEC §6).
func (s *Server) TeacherRecheck(ctx context.Context, actor store.User, targetUserID int64, taskID string) (store.Submission, error) {
	if actor.Role != "teacher" {
		return store.Submission{}, fmt.Errorf("recheck: %q is not a teacher", actor.Login)
	}
	sub, _, err := s.recheck(ctx, targetUserID, taskID, true)
	if err != nil {
		return store.Submission{}, err
	}
	target, terr := s.DB.GetUserByID(ctx, targetUserID)
	if terr == nil {
		_ = s.DB.Log(ctx, store.Event{
			ActorID: &actor.ID, Kind: "recheck",
			Target: target.Login + "/" + taskID,
			Detail: fmt.Sprintf("teacher recheck #%d", sub.ID),
		})
	}
	return sub, nil
}

func (s *Server) recheck(ctx context.Context, userID int64, taskID string, teacher bool) (store.Submission, queue.Decision, error) {
	task, _, ok := s.Course.Get().Task(taskID)
	if !ok {
		return store.Submission{}, queue.Decision{}, fmt.Errorf("unknown task %q", taskID)
	}
	user, err := s.DB.GetUserByID(ctx, userID)
	if err != nil {
		return store.Submission{}, queue.Decision{}, err
	}
	// This read only picks which commit to re-grade; the attempt/cooldown
	// decision is taken again inside Submit's transaction.
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

	sub, d, err := s.Queue.Submit(ctx, task, store.NewSubmission{
		UserID: userID, TaskID: taskID, CommitSHA: commit, ReceivedAt: time.Now(),
	}, teacher)
	if err != nil {
		return store.Submission{}, d, err
	}
	if !d.Admit {
		return sub, d, nil
	}
	// Pin the rechecked commit like a push does (SPEC §6 step 7). There is no
	// push output here, so a failure is logged and the recheck stands.
	_ = s.pinSubmission(ctx, s.Repos.StudentDir(user.Login), sub.ID, commit)
	return sub, d, nil
}
