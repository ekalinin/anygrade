package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/ratelimit"
	"github.com/ekalinin/anygrade/internal/store"
)

// newAPIUser creates an account and returns it with the plaintext of its
// personal token - the same credential the login form and git basic auth take
// (SPEC §8).
func newAPIUser(t *testing.T, h *Handler, login, role string) (store.User, string) {
	t.Helper()
	u, err := h.DB.CreateUser(t.Context(), login, strings.ToUpper(login), role)
	if err != nil {
		t.Fatalf("create user %s: %v", login, err)
	}
	token, err := h.DB.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("issue token for %s: %v", login, err)
	}
	return u, token
}

// apiDo issues one request with a literal Authorization header ("" sends none).
func apiDo(h *Handler, target, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	return rec
}

// apiGet is apiDo with the token presented the way a client should.
func apiGet(h *Handler, target, token string) *httptest.ResponseRecorder {
	if token == "" {
		return apiDo(h, target, "")
	}
	return apiDo(h, target, "Bearer "+token)
}

// apiObject decodes one JSON object response and fails on anything else - an
// HTML error page reaching a client is exactly what these tests are about.
func apiObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q, want application/json:\n%s", ct, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body: %v\n%s", err, rec.Body.String())
	}
	return m
}

// wantKeys pins one object's field set. The field set is the contract: a field
// that disappears has to fail here rather than silently at a client.
func wantKeys(t *testing.T, what string, obj map[string]any, keys ...string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	slices.Sort(got)
	slices.Sort(keys)
	if !slices.Equal(got, keys) {
		t.Errorf("%s: fields %v, want %v", what, got, keys)
	}
}

// objectAt walks into a nested object (or the index-th element of an array).
func objectAt(t *testing.T, what string, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: %#v is not an object", what, v)
	}
	return m
}

func arrayAt(t *testing.T, what string, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: %#v is not an array", what, v)
	}
	return a
}

// apiErrCode reads the code out of an error body, so a test asserts the
// machine-readable half rather than the prose.
func apiErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	code, ok := objectAt(t, "error", apiObject(t, rec)["error"])["code"].(string)
	if !ok {
		t.Fatalf("error body has no string code:\n%s", rec.Body.String())
	}
	return code
}

// finishSub enqueues one submission and drives it through the worker's path -
// claim, then finish - so the read model has real results behind it. ClaimNext
// hands out the oldest eligible row, which is this one as long as callers do
// not leave others queued.
func finishSub(t *testing.T, h *Handler, userID int64, taskID string, res store.SubmissionResult) int64 {
	t.Helper()
	id := enqueue(t, h, userID, taskID, time.Now())
	claimed, ok, err := h.DB.ClaimNext(t.Context(), time.Now())
	if err != nil || !ok || claimed.ID != id {
		t.Fatalf("claim %d: got #%d ok=%v err=%v", id, claimed.ID, ok, err)
	}
	if err := h.DB.FinishSubmission(t.Context(), id, res); err != nil {
		t.Fatalf("finish %d: %v", id, err)
	}
	return id
}

// TestAPIRoleEndpointMatrix is the authorization table: exactly what each role
// gets from each endpoint. The row that matters most is a student reaching
// another student's submission - 404, never 403, so an id cannot be probed
// (SPEC §14).
//
// The TA rows are the point of the API being a second encoder rather than a
// second set of rules: a TA reads the matrix, the queue and every submission
// in the UI, so the API answers the same, and nothing more.
func TestAPIRoleEndpointMatrix(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, profTok := newAPIUser(t, h, "prof", "teacher")
	_, taTok := newAPIUser(t, h, "tanya", store.RoleTA)
	alice, aliceTok := newAPIUser(t, h, "alice", "student")
	bob, bobTok := newAPIUser(t, h, "bob", "student")
	aliceSub := enqueue(t, h, alice.ID, "t1", time.Now())
	bobSub := enqueue(t, h, bob.ID, "t1", time.Now())

	tests := []struct {
		name   string
		target string
		token  string
		want   int
	}{
		{"student reads own identity", "/api/v1/me", aliceTok, http.StatusOK},
		{"teacher reads own identity", "/api/v1/me", profTok, http.StatusOK},
		{"student reads own tasks", "/api/v1/tasks", aliceTok, http.StatusOK},
		{"teacher reads own tasks", "/api/v1/tasks", profTok, http.StatusOK},
		{"student reads own submission",
			fmt.Sprintf("/api/v1/submissions/%d", aliceSub), aliceTok, http.StatusOK},
		{"student reaching another student's submission",
			fmt.Sprintf("/api/v1/submissions/%d", bobSub), aliceTok, http.StatusNotFound},
		{"the owner of that submission",
			fmt.Sprintf("/api/v1/submissions/%d", bobSub), bobTok, http.StatusOK},
		{"teacher reads any submission",
			fmt.Sprintf("/api/v1/submissions/%d", bobSub), profTok, http.StatusOK},
		{"ta reads any submission",
			fmt.Sprintf("/api/v1/submissions/%d", bobSub), taTok, http.StatusOK},
		{"submission that does not exist", "/api/v1/submissions/9999", profTok, http.StatusNotFound},
		{"submission id that is not a number", "/api/v1/submissions/x", profTok, http.StatusNotFound},
		{"ta reads own identity", "/api/v1/me", taTok, http.StatusOK},
		{"student reaching the matrix", "/api/v1/matrix", aliceTok, http.StatusNotFound},
		{"teacher reads the matrix", "/api/v1/matrix", profTok, http.StatusOK},
		{"ta reads the matrix", "/api/v1/matrix", taTok, http.StatusOK},
		{"student reaching the queue", "/api/v1/queue", aliceTok, http.StatusNotFound},
		{"teacher reads the queue", "/api/v1/queue", profTok, http.StatusOK},
		{"ta reads the queue", "/api/v1/queue", taTok, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := apiGet(h, tc.target, tc.token)
			if rec.Code != tc.want {
				t.Fatalf("GET %s: status %d, want %d\n%s", tc.target, rec.Code, tc.want, rec.Body.String())
			}
			// Even a refusal is JSON: a client parses the code, and an HTML
			// page here would mean the site's error path leaked into the API.
			if tc.want == http.StatusNotFound {
				if code := apiErrCode(t, rec); code != codeNotFound {
					t.Errorf("GET %s: error code %q, want %q", tc.target, code, codeNotFound)
				}
			}
		})
	}
}

// TestAPIIdentityFollowsTheToken: a token belonging to another user answers as
// that user and no further - the bearer is the whole identity.
func TestAPIIdentityFollowsTheToken(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, aliceTok := newAPIUser(t, h, "alice", "student")
	_, bobTok := newAPIUser(t, h, "bob", "student")

	for token, want := range map[string]string{aliceTok: "alice", bobTok: "bob"} {
		rec := apiGet(h, "/api/v1/me", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/me: status %d", rec.Code)
		}
		if got := apiObject(t, rec)["login"]; got != want {
			t.Errorf("GET /api/v1/me: login %v, want %q", got, want)
		}
	}
}

// TestAPIRejectsBadCredentials covers every way a request can fail to carry a
// usable token. All of them are a JSON 401 - never a redirect to the login
// form, which a script cannot follow.
func TestAPIRejectsBadCredentials(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, aliceTok := newAPIUser(t, h, "alice", "student")
	gone, goneTok := newAPIUser(t, h, "gone", "student")
	if err := h.DB.SetUserState(t.Context(), gone.Login, "disabled"); err != nil {
		t.Fatalf("disable %s: %v", gone.Login, err)
	}

	tests := []struct {
		name   string
		header string
	}{
		{"no Authorization header", ""},
		{"bearer with no token", "Bearer"},
		{"bearer with an empty token", "Bearer   "},
		{"another scheme", "Basic YWxpY2U6c2VjcmV0"},
		{"the token without a scheme", aliceTok},
		{"a token that was never issued", "Bearer ag_" + strings.Repeat("0", 64)},
		{"a valid token of a disabled account", "Bearer " + goneTok},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := apiDo(h, "/api/v1/tasks", tc.header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET /api/v1/tasks: status %d, want 401\n%s", rec.Code, rec.Body.String())
			}
			if code := apiErrCode(t, rec); code != codeUnauthorized {
				t.Errorf("error code %q, want %q", code, codeUnauthorized)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("Location %q: the API must never redirect to the login form", loc)
			}
			if c := rec.Header().Get("Set-Cookie"); c != "" {
				t.Errorf("Set-Cookie %q: the API must not touch the session", c)
			}
		})
	}
}

// TestAPIIgnoresTheSessionCookie: a browser session is not an API credential.
// The two surfaces share the token, not the cookie.
func TestAPIIgnoresTheSessionCookie(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, cookie := newSession(t, h, "alice", "student")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/tasks with a session cookie: status %d, want 401\n%s",
			rec.Code, rec.Body.String())
	}
}

// TestAPINeverSetsACookieOrRedirects: authenticated or not, an API response
// carries no session and no navigation. The same unauthenticated GET on the
// page routes is a 302 to /login.
func TestAPINeverSetsACookieOrRedirects(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, aliceTok := newAPIUser(t, h, "alice", "student")

	for _, token := range []string{"", aliceTok} {
		rec := apiGet(h, "/api/v1/tasks", token)
		if rec.Code == http.StatusFound || rec.Code == http.StatusSeeOther {
			t.Errorf("token %q: status %d is a redirect", token, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("token %q: Location %q", token, loc)
		}
		if c := rec.Header().Get("Set-Cookie"); c != "" {
			t.Errorf("token %q: Set-Cookie %q", token, c)
		}
	}
	// The contrast: the page route redirects the same anonymous request.
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /: status %d, want the login redirect the API refuses to serve", rec.Code)
	}
}

// TestAPILocalModeNeedsNoToken: `serve --local` has one implicit user and no
// credentials at all (SPEC §8), and the API follows the pages rather than
// inventing a login for itself. The bypass stays unreachable unless the
// composition root sets Local - the zero value is the secure one.
func TestAPILocalModeNeedsNoToken(t *testing.T) {
	h, u := newTestSite(t)
	setCourse(h)
	h.Local = &u

	body := apiObject(t, apiGet(h, "/api/v1/me", ""))
	if body["login"] != u.Login {
		t.Errorf("local mode GET /api/v1/me: login %v, want %q", body["login"], u.Login)
	}
	// The implicit user is a teacher, so the teacher endpoints answer too.
	if rec := apiGet(h, "/api/v1/matrix", ""); rec.Code != http.StatusOK {
		t.Errorf("local mode GET /api/v1/matrix: status %d, want 200", rec.Code)
	}
}

// TestAPIBadBearerFeedsTheLimiter: a bearer failure is a credential failure and
// draws on the budget the login form and git basic auth share, or the API is
// the unthrottled way to guess a token (SPEC §14).
func TestAPIBadBearerFeedsTheLimiter(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	h.Limit = ratelimit.New(2, time.Minute)

	for i := range 2 {
		if rec := apiGet(h, "/api/v1/tasks", "ag_wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i, rec.Code)
		}
	}
	rec := apiGet(h, "/api/v1/tasks", "ag_wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after the budget: status %d, want 429\n%s", rec.Code, rec.Body.String())
	}
	if code := apiErrCode(t, rec); code != codeRateLimited {
		t.Errorf("error code %q, want %q", code, codeRateLimited)
	}
}

// TestAPIGoodBearerDoesNotFeedTheLimiter: only failures count, so ordinary
// polling by a script with a valid token is never throttled.
func TestAPIGoodBearerDoesNotFeedTheLimiter(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	h.Limit = ratelimit.New(1, time.Minute) // one failure is already too many
	_, aliceTok := newAPIUser(t, h, "alice", "student")

	for i := range 3 {
		if rec := apiGet(h, "/api/v1/tasks", aliceTok); rec.Code != http.StatusOK {
			t.Fatalf("valid request %d: status %d, want 200", i, rec.Code)
		}
	}
	// The budget was never spent, so the first bad token still gets its answer
	// - and the second one runs into the wall.
	if rec := apiGet(h, "/api/v1/tasks", "ag_wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("first bad token: status %d, want 401", rec.Code)
	}
	if rec := apiGet(h, "/api/v1/tasks", "ag_wrong"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second bad token: status %d, want 429", rec.Code)
	}
}

// TestAPISubmissionNoteIsViewerScoped: the note the API hands out is the one
// the page hands out - the student-safe projection for its owner, the
// operator's own for the teacher (SPEC §14).
func TestAPISubmissionNoteIsViewerScoped(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, profTok := newAPIUser(t, h, "prof", "teacher")
	alice, aliceTok := newAPIUser(t, h, "alice", "student")

	const workerNote = "docker: pull access denied for course/image"
	id := enqueue(t, h, alice.ID, "t1", time.Now())
	if _, ok, err := h.DB.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if ok, err := h.DB.ScheduleRetry(t.Context(), id, nil, workerNote, ""); err != nil || !ok {
		t.Fatalf("schedule retry: ok=%v err=%v", ok, err)
	}

	target := fmt.Sprintf("/api/v1/submissions/%d", id)
	if got := apiObject(t, apiGet(h, target, aliceTok))["note"]; got != "" {
		t.Errorf("owner's note %q, want empty: operator detail stays with the teacher", got)
	}
	if got := apiObject(t, apiGet(h, target, profTok))["note"]; got != workerNote {
		t.Errorf("teacher's note %q, want %q", got, workerNote)
	}
}

// TestAPIMatrixCellStatusNeverEmpty: gradebook blanks an untouched cell so the
// page can draw a dash there. A client cannot branch on an empty string, so the
// encoder restores the name.
func TestAPIMatrixCellStatusNeverEmpty(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, profTok := newAPIUser(t, h, "prof", "teacher")
	newAPIUser(t, h, "alice", "student")

	body := apiObject(t, apiGet(h, "/api/v1/matrix", profTok))
	row := objectAt(t, "rows[0]", arrayAt(t, "rows", body["rows"])[0])
	cell := objectAt(t, "cells.t1", objectAt(t, "cells", row["cells"])["t1"])
	if cell["status"] != gradebook.StatusNotStarted {
		t.Errorf("untouched cell status %v, want %q", cell["status"], gradebook.StatusNotStarted)
	}
	if cell["latest_submission_id"] != nil {
		t.Errorf("untouched cell latest_submission_id %v, want null", cell["latest_submission_id"])
	}
}

// TestAPIResponseShapes pins the field set of every endpoint. Adding a field is
// allowed inside /api/v1/ and will fail here on purpose: the list below is the
// promise, so extending it is a deliberate edit rather than a leak.
func TestAPIResponseShapes(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, profTok := newAPIUser(t, h, "prof", "teacher")
	alice, aliceTok := newAPIUser(t, h, "alice", "student")

	score := 7.0
	done := finishSub(t, h, alice.ID, "t1", store.SubmissionResult{
		Status: store.StatusDone, Raw: score, Final: score,
		Checks: []store.CheckRow{{
			Name: "unit", Passed: true, Weight: 1,
			Duration: 1500 * time.Millisecond, LogExcerpt: "ok",
		}},
	})
	if err := h.DB.SetScoreOverride(t.Context(), store.ScoreOverride{
		UserID: alice.ID, TaskID: "t2", Score: 9, Comment: "graded by hand", TeacherID: 1,
	}); err != nil {
		t.Fatalf("set override: %v", err)
	}
	// One row left queued, so the teacher's queue has something in it.
	enqueue(t, h, alice.ID, "t2", time.Now())

	t.Run("me", func(t *testing.T) {
		body := apiObject(t, apiGet(h, "/api/v1/me", aliceTok))
		wantKeys(t, "me", body, "login", "display_name", "role")
		if body["role"] != "student" {
			t.Errorf("role %v, want student", body["role"])
		}
	})

	t.Run("tasks", func(t *testing.T) {
		body := apiObject(t, apiGet(h, "/api/v1/tasks", aliceTok))
		wantKeys(t, "tasks", body, "course", "tasks")
		tasks := arrayAt(t, "tasks", body["tasks"])
		if len(tasks) != 2 {
			t.Fatalf("tasks: %d rows, want the course's 2", len(tasks))
		}
		first := objectAt(t, "tasks[0]", tasks[0])
		wantKeys(t, "tasks[0]", first,
			"id", "name", "max_score", "status", "score", "computed_score",
			"override", "attempts", "latest_submission_id", "soft_deadline", "hard_deadline")
		if first["score"] != score || first["status"] != "partial" {
			t.Errorf("tasks[0]: score %v status %v, want %v partial", first["score"], first["status"], score)
		}
		if first["latest_submission_id"] != float64(done) {
			t.Errorf("tasks[0]: latest_submission_id %v, want %d", first["latest_submission_id"], done)
		}
		// The overridden task carries both numbers, the way the page prints
		// them: the teacher's score wins and the machine's stays visible.
		second := objectAt(t, "tasks[1]", tasks[1])
		wantKeys(t, "tasks[1].override", objectAt(t, "override", second["override"]), "score", "comment")
		if second["score"] != 9.0 || second["computed_score"] != nil {
			t.Errorf("tasks[1]: score %v computed %v, want 9 and null",
				second["score"], second["computed_score"])
		}
	})

	t.Run("submission", func(t *testing.T) {
		body := apiObject(t, apiGet(h, fmt.Sprintf("/api/v1/submissions/%d", done), aliceTok))
		wantKeys(t, "submission", body,
			"id", "task_id", "task_name", "max_score", "commit", "status",
			"running", "rejected", "attempt_no", "counts", "received_at", "started_at",
			"raw_score", "penalty_percent", "final_score", "note", "checks")
		if body["status"] != store.StatusDone || body["final_score"] != score {
			t.Errorf("submission: status %v final_score %v", body["status"], body["final_score"])
		}
		checks := arrayAt(t, "checks", body["checks"])
		if len(checks) != 1 {
			t.Fatalf("checks: %d rows, want 1", len(checks))
		}
		check := objectAt(t, "checks[0]", checks[0])
		wantKeys(t, "checks[0]", check,
			"name", "passed", "exit_code", "duration_ms", "weight",
			"skipped", "timed_out", "build_failed", "log_excerpt")
		if check["duration_ms"] != 1500.0 {
			t.Errorf("checks[0]: duration_ms %v, want 1500", check["duration_ms"])
		}
	})

	t.Run("matrix", func(t *testing.T) {
		body := apiObject(t, apiGet(h, "/api/v1/matrix", profTok))
		wantKeys(t, "matrix", body, "tasks", "rows")
		wantKeys(t, "matrix.tasks[0]", objectAt(t, "tasks[0]", arrayAt(t, "tasks", body["tasks"])[0]),
			"id", "name", "max_score")
		rows := arrayAt(t, "rows", body["rows"])
		if len(rows) != 1 {
			t.Fatalf("rows: %d, want the one student", len(rows))
		}
		row := objectAt(t, "rows[0]", rows[0])
		wantKeys(t, "matrix.rows[0]", row, "login", "display_name", "total", "cells")
		cells := objectAt(t, "cells", row["cells"])
		cell := objectAt(t, "cells.t1", cells["t1"])
		wantKeys(t, "matrix cell", cell,
			"status", "score", "computed_score", "override_score", "latest_submission_id")
		if cell["score"] != score || cell["latest_submission_id"] != float64(done) {
			t.Errorf("cells.t1: score %v latest %v", cell["score"], cell["latest_submission_id"])
		}
		if got := objectAt(t, "cells.t2", cells["t2"])["override_score"]; got != 9.0 {
			t.Errorf("cells.t2 override_score %v, want 9", got)
		}
	})

	t.Run("queue", func(t *testing.T) {
		body := apiObject(t, apiGet(h, "/api/v1/queue", profTok))
		wantKeys(t, "queue", body, "rows")
		rows := arrayAt(t, "rows", body["rows"])
		if len(rows) != 1 {
			t.Fatalf("queue rows: %d, want the one queued submission", len(rows))
		}
		row := objectAt(t, "rows[0]", rows[0])
		wantKeys(t, "queue.rows[0]", row,
			"id", "login", "task_id", "status", "received_at", "started_at",
			"attempt_no", "retries", "note")
		if row["login"] != "alice" || row["status"] != store.StatusQueued {
			t.Errorf("queue row: login %v status %v", row["login"], row["status"])
		}
	})
}
