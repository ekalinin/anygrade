package intake

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/hookproto"
)

// pushCourse commits the course source and pushes it into the mirror, then
// returns the update the teacher's hooks would report.
func pushCourse(t *testing.T, s *Server, courseSrc, msg string) hookproto.RefUpdate {
	t.Helper()
	old := git(t, s.Repos.CourseDir(), "rev-parse", "refs/heads/main")
	head := commitAll(t, courseSrc, msg)
	if err := s.Repos.EnsureCourse(t.Context(), courseSrc); err != nil {
		t.Fatal(err)
	}
	return hookproto.RefUpdate{Old: old, New: head, Ref: "refs/heads/main"}
}

// TestValidateCourseAppliesServeGate: the §14 gate ran only at startup, so a
// teacher push switching a task to the local runner turned the unsandboxed
// runner on behind the --allow-local-runner flag's back. Schema-valid
// metadata this process cannot serve is rejected like any other bad push.
func TestValidateCourseAppliesServeGate(t *testing.T) {
	s, _, courseSrc, _ := newIntakeFixture(t)
	s.Safety = func(*config.Resolved) error {
		return errors.New("task(s) t1 use the local runner")
	}

	u := pushCourse(t, s, courseSrc, "teacher edit")
	resp := s.dispatch(t.Context(), hookproto.Request{
		Kind: hookproto.KindValidateCourse, Repo: "course", Actor: "prof", Role: "teacher",
		Updates: []hookproto.RefUpdate{u},
	})
	if resp.ExitCode == 0 {
		t.Fatalf("the push must be rejected, got %+v", resp)
	}
	if out := joined(resp); !strings.Contains(out, "local runner") {
		t.Errorf("rejection %q must carry the gate's reason", out)
	}

	// A snapshot the gate accepts still goes through.
	s.Safety = func(*config.Resolved) error { return nil }
	if resp := s.dispatch(t.Context(), hookproto.Request{
		Kind: hookproto.KindValidateCourse, Repo: "course", Actor: "prof", Role: "teacher",
		Updates: []hookproto.RefUpdate{u},
	}); resp.ExitCode != 0 {
		t.Fatalf("an acceptable snapshot must pass, got %+v", resp)
	}
}

// TestCourseUpdatedRefusesUnsafeSnapshot: pre-receive is not the only way into
// the mirror (a direct write, a repo whose hook is gone), so the swap itself
// has to refuse too - and refusing means the vetted snapshot stays live.
func TestCourseUpdatedRefusesUnsafeSnapshot(t *testing.T) {
	s, _, courseSrc, _ := newIntakeFixture(t)
	before := s.Course.Get()
	s.Safety = func(*config.Resolved) error {
		return errors.New("task(s) t1 use the local runner")
	}

	pushCourse(t, s, courseSrc, "teacher edit")
	resp := s.dispatch(t.Context(), hookproto.Request{
		Kind: hookproto.KindPostReceive, Repo: "course", Actor: "prof", Role: "teacher",
	})
	if out := joined(resp); !strings.Contains(out, "previous version stays active") {
		t.Errorf("response %q must say the snapshot was kept", out)
	}
	if s.Course.Get() != before {
		t.Fatal("the refused snapshot went live anyway")
	}

	// Once the gate is satisfied the reload lands as before.
	s.Safety = func(*config.Resolved) error { return nil }
	if resp := s.dispatch(t.Context(), hookproto.Request{
		Kind: hookproto.KindPostReceive, Repo: "course", Actor: "prof", Role: "teacher",
	}); !strings.Contains(joined(resp), "course metadata reloaded") {
		t.Fatalf("an acceptable snapshot must load, got %+v", resp)
	}
	if s.Course.Get() == before {
		t.Fatal("the accepted snapshot did not go live")
	}
}

// TestCourseUpdatedAdoptsMaxPushSize: the cap is course metadata, so a teacher
// who changes it must not need a restart (SPEC §13).
func TestCourseUpdatedAdoptsMaxPushSize(t *testing.T) {
	s, _, courseSrc, _ := newIntakeFixture(t)
	if got := s.Repos.MaxInputSize(); got != config.DefaultMaxPushSize {
		t.Fatalf("initial cap = %d, want the default %d", got, config.DefaultMaxPushSize)
	}

	appendCourseYAML(t, courseSrc, "limits:\n  max_push_size: 4m\n")
	pushCourse(t, s, courseSrc, "shrink max_push_size")
	if resp := s.dispatch(t.Context(), hookproto.Request{
		Kind: hookproto.KindPostReceive, Repo: "course", Actor: "prof", Role: "teacher",
	}); !strings.Contains(joined(resp), "course metadata reloaded") {
		t.Fatalf("reload failed: %+v", resp)
	}
	if got := s.Repos.MaxInputSize(); got != 4<<20 {
		t.Fatalf("cap after the push = %d, want %d", got, 4<<20)
	}
}

// appendCourseYAML adds a block to the fixture's course.yaml.
func appendCourseYAML(t *testing.T, courseSrc, block string) {
	t.Helper()
	path := filepath.Join(courseSrc, "course.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte(block)...), 0o644); err != nil {
		t.Fatal(err)
	}
}
