package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

// newOverrideSite builds a one-task course, a teacher (the implicit local
// user), and a student with one graded submission worth 4 of 10.
func newOverrideSite(t *testing.T) (*Handler, store.User, store.User) {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	teacher, err := db.CreateUser(t.Context(), "teach", "Teacher", "teacher")
	if err != nil {
		t.Fatalf("create teacher: %v", err)
	}
	student, err := db.CreateUser(t.Context(), "stud", "Student", "student")
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	sub, err := db.Enqueue(t.Context(), store.NewSubmission{
		UserID: student.ID, TaskID: "t1", CommitSHA: "deadbeef",
		ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := db.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	if err := db.FinishSubmission(t.Context(), sub.ID, store.SubmissionResult{
		Status: store.StatusDone, Raw: 4, Final: 4,
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	holder := &intake.Holder{}
	holder.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{Name: "Test course", ScoringPolicy: "best"},
		Tasks:  []config.ResolvedTask{{ID: "t1", Name: "Task one", Score: 10}},
	}})
	h := &Handler{DB: db, Course: holder, Hub: NewHub(), Local: &teacher}
	h.ReadCourseFile = func(context.Context, string, string) ([]byte, bool, error) {
		return nil, false, nil // no README in the fixture course
	}
	return h, teacher, student
}

// storeOverride writes a manual score straight to the store (the display
// tests are about rendering, not about the POST handler).
func storeOverride(t *testing.T, h *Handler, teacher, student store.User, score float64, comment string) {
	t.Helper()
	if err := h.DB.SetScoreOverride(t.Context(), store.ScoreOverride{
		UserID: student.ID, TaskID: "t1", Score: score,
		Comment: comment, TeacherID: teacher.ID,
	}); err != nil {
		t.Fatalf("set override: %v", err)
	}
}

// postOverride submits the teacher's override form.
func postOverride(t *testing.T, h *Handler, score, comment string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"score": {score}, "comment": {comment}}
	req := httptest.NewRequest(http.MethodPost,
		"/students/stud/tasks/t1/override", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	return rec
}

// TestDashboardShowsOverride: the student's own dashboard shows the manual
// score, not the computed one (SPEC §9).
func TestDashboardShowsOverride(t *testing.T) {
	h, teacher, student := newOverrideSite(t)
	storeOverride(t, h, teacher, student, 9, "manual review")
	h.Local = &student

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The correction shows both numbers: the machine's, struck through, and
	// the teacher's in pen. The override is still the score that counts -
	// Display(), Total and the CSV export are unchanged (SPEC §9). The struck
	// number is provenance (SPEC.ui.md 3.1).
	if !strings.Contains(body, `class="pen">9<`) {
		t.Errorf("GET /: override score not shown in pen:\n%s", body)
	}
	if !strings.Contains(body, `class="machine">4<`) {
		t.Errorf("GET /: superseded computed score not struck through:\n%s", body)
	}
	if !strings.Contains(body, "set by teacher") {
		t.Errorf("GET /: no override marker:\n%s", body)
	}
	if !strings.Contains(body, "manual review") {
		t.Errorf("GET /: override comment missing:\n%s", body)
	}
}

// TestTaskPageShowsOverride: same for the task page header.
func TestTaskPageShowsOverride(t *testing.T) {
	h, teacher, student := newOverrideSite(t)
	storeOverride(t, h, teacher, student, 9, "manual review")
	h.Local = &student

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks/t1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks/t1: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="pen">9<`) {
		t.Errorf("GET /tasks/t1: override score not shown in pen:\n%s", body)
	}
	if !strings.Contains(body, "set by teacher: manual review") {
		t.Errorf("GET /tasks/t1: no override marker with comment:\n%s", body)
	}
	// The history table keeps the raw per-submission score.
	if !strings.Contains(body, "<td>4</td>") {
		t.Errorf("GET /tasks/t1: submission history lost its own score:\n%s", body)
	}
}

// TestDashboardWithoutOverrideShowsComputed: the computed score still wins
// when no teacher touched the task.
func TestDashboardWithoutOverrideShowsComputed(t *testing.T) {
	h, _, student := newOverrideSite(t)
	h.Local = &student

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	// The computed score is unwrapped text right before the denominator span
	// (no "machine"/"pen" markup - that only appears once a teacher overrides).
	if !regexp.MustCompile(`>\s*4\s*<span class="den">/ 10<`).MatchString(body) {
		t.Errorf("GET /: computed score not shown:\n%s", body)
	}
	if strings.Contains(body, "set by teacher") {
		t.Errorf("GET /: unexpected override marker:\n%s", body)
	}
}

// TestSetOverrideRequiresComment: SPEC §9 wants a justification, so an empty
// or whitespace-only comment is rejected and nothing is written.
func TestSetOverrideRequiresComment(t *testing.T) {
	for name, comment := range map[string]string{"empty": "", "spaces": "   \t "} {
		t.Run(name, func(t *testing.T) {
			h, _, student := newOverrideSite(t)

			rec := postOverride(t, h, "9", comment)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST override: status %d, want 400", rec.Code)
			}
			if body := rec.Body.String(); !strings.Contains(body, "comment is required") {
				t.Errorf("POST override: unexpected message %q", body)
			}
			if _, ok, err := h.DB.GetScoreOverride(t.Context(), student.ID, "t1"); err != nil || ok {
				t.Errorf("override written despite an empty comment (ok=%v, err=%v)", ok, err)
			}
		})
	}
}

// TestSetOverrideStoresTrimmedComment: a valid override still goes through,
// with the comment trimmed.
func TestSetOverrideStoresTrimmedComment(t *testing.T) {
	h, _, student := newOverrideSite(t)

	rec := postOverride(t, h, "9", "  manual review  ")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST override: status %d, want 303", rec.Code)
	}
	o, ok, err := h.DB.GetScoreOverride(t.Context(), student.ID, "t1")
	if err != nil || !ok {
		t.Fatalf("override not stored: ok=%v, err=%v", ok, err)
	}
	if o.Score != 9 || o.Comment != "manual review" {
		t.Errorf("stored override: %+v", o)
	}
}

// TestSetOverrideCommentMessageLocalized: the 400 body follows the UI locale.
func TestSetOverrideCommentMessageLocalized(t *testing.T) {
	h, _, _ := newOverrideSite(t)

	form := url.Values{"score": {"9"}, "comment": {""}}
	req := httptest.NewRequest(http.MethodPost,
		"/students/stud/tasks/t1/override", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "ru"})
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST override: status %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "нужен комментарий") {
		t.Errorf("POST override [ru]: message not translated: %q", body)
	}
}

// TestStudentPageShowsOverride: the teacher's view keeps both numbers - the
// computed score in its column, the manual one in the override column.
func TestStudentPageShowsOverride(t *testing.T) {
	h, teacher, student := newOverrideSite(t)
	storeOverride(t, h, teacher, student, 9, "manual review")

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/students/stud", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /students/stud: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "4 / 10") {
		t.Errorf("GET /students/stud: computed score column lost:\n%s", body)
	}
	if !strings.Contains(body, "manual review") {
		t.Errorf("GET /students/stud: override column lost:\n%s", body)
	}
}

// TestTaskPageShowsTheCorrection: the task page carries the same correction
// gesture as the dashboard - the machine's number struck through, the
// teacher's in pen, the comment as a margin note (SPEC.ui.md 3.1).
func TestTaskPageShowsTheCorrection(t *testing.T) {
	h, teacher, student := newOverrideSite(t)
	storeOverride(t, h, teacher, student, 9, "manual review")
	h.Local = &student

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks/t1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks/t1: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="machine">4<`,
		`class="pen">9<`,
		`class="note"`,
		"manual review",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("task page: missing %s:\n%s", want, body)
		}
	}
}

// TestTaskPageWithoutOverrideHasNoPen: red means a human intervened, so a task
// the machine graded on its own carries no pen markup.
func TestTaskPageWithoutOverrideHasNoPen(t *testing.T) {
	h, _, student := newOverrideSite(t)
	h.Local = &student

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks/t1", nil))

	body := rec.Body.String()
	for _, unwanted := range []string{`class="pen"`, `class="machine"`, `class="note"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("task page: %s must not appear without an override:\n%s", unwanted, body)
		}
	}
}

// TestMatrixOverrideWithoutSubmissions: a task a teacher scored by hand and
// the student never submitted to. The score enters the row total, so the cell
// has to show it. It used to be drawn as a dash - the status was blanked as
// "not started" while the total counted the score anyway, and the row did not
// add up to what was in it.
func TestMatrixOverrideWithoutSubmissions(t *testing.T) {
	h, teacher, student := newOverrideSite(t)
	h.Course.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{Name: "Test course", ScoringPolicy: "best"},
		Tasks: []config.ResolvedTask{
			{ID: "t1", Name: "Task one", Score: 10},
			{ID: "t2", Name: "Task two", Score: 5},
		},
	}})
	if err := h.DB.SetScoreOverride(t.Context(), store.ScoreOverride{
		UserID: student.ID, TaskID: "t2", Score: 5,
		Comment: "credited from the seminar", TeacherID: teacher.ID,
	}); err != nil {
		t.Fatalf("set override: %v", err)
	}

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/matrix", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /matrix: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `title="overridden"`) {
		t.Errorf("GET /matrix: the hand-scored cell is not rendered as a score:\n%s", body)
	}
	// 4 computed on t1 plus 5 overridden on t2, and both are visible.
	if !strings.Contains(body, "<strong>9</strong>") {
		t.Errorf("GET /matrix: row total missing or wrong:\n%s", body)
	}

	// The student's own dashboard says the same thing.
	h.Local = &student
	rec = httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "overridden") {
		t.Errorf("GET /: the hand-scored task still reads as untouched:\n%s", body)
	}
}
