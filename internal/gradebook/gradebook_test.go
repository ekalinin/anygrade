package gradebook

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/store"
)

func sub(status string, final *float64, counts bool) store.Submission {
	return store.Submission{Status: status, FinalScore: final, Counts: counts}
}

func TestDeriveStatus(t *testing.T) {
	retryAt := time.Now().Add(time.Minute)
	canceledAt := time.Now()
	tests := []struct {
		name    string
		history []store.Submission
		want    string
	}{
		{"empty", nil, StatusNotStarted},
		{"queued", []store.Submission{sub(store.StatusQueued, nil, true)}, "queued"},
		{"running", []store.Submission{sub(store.StatusRunning, nil, true)}, "running"},
		{"passed", []store.Submission{sub(store.StatusDone, new(float64(100)), true)}, StatusPassed},
		{"partial", []store.Submission{sub(store.StatusDone, new(float64(60)), true)}, StatusPartial},
		{"failed", []store.Submission{sub(store.StatusDone, new(float64(0)), true)}, StatusFailed},
		{"rejected", []store.Submission{sub(store.StatusRejectedDeadline, nil, true)}, StatusRejected},
		{"latest wins", []store.Submission{
			sub(store.StatusDone, new(float64(100)), true),
			sub(store.StatusQueued, nil, true),
		}, "queued"},
		{"retrying", []store.Submission{{Status: store.StatusInfraError, RetryAt: &retryAt}}, StatusRetrying},
		{"terminal infra", []store.Submission{{Status: store.StatusInfraError}}, StatusError},
		{"canceled", []store.Submission{{Status: store.StatusInfraError, CanceledAt: &canceledAt}}, StatusCanceled},
	}
	for _, tc := range tests {
		if got := DeriveStatus(tc.history, 100); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDisplayScore(t *testing.T) {
	history := []store.Submission{
		sub(store.StatusDone, new(float64(80)), true),
		sub(store.StatusRejectedLimit, nil, true),
		sub(store.StatusDone, new(float64(50)), true),
	}
	if got := DisplayScore(history, "best"); got == nil || *got != 80 {
		t.Errorf("best: %v", got)
	}
	if got := DisplayScore(history, "latest"); got == nil || *got != 50 {
		t.Errorf("latest: %v", got)
	}
	if got := DisplayScore(nil, "best"); got != nil {
		t.Errorf("empty: %v", got)
	}
}

func TestBuildMatrix(t *testing.T) {
	users := []store.User{
		{ID: 2, Login: "bob", Role: "student"},
		{ID: 1, Login: "alice", Role: "student"},
		{ID: 3, Login: "prof", Role: "teacher"},
	}
	tasks := []TaskCol{{ID: "t1", Name: "T1", MaxScore: 100}, {ID: "t2", Name: "T2", MaxScore: 50}}
	subs := []store.Submission{
		{ID: 10, UserID: 1, TaskID: "t1", Status: store.StatusDone, FinalScore: new(float64(60)), Counts: true},
		{ID: 11, UserID: 1, TaskID: "t1", Status: store.StatusDone, FinalScore: new(float64(80)), Counts: true},
		{ID: 12, UserID: 2, TaskID: "t2", Status: store.StatusQueued, Counts: true},
	}
	overrides := []store.ScoreOverride{{UserID: 1, TaskID: "t2", Score: 33, TeacherID: 3}}

	m := Build(users, tasks, subs, overrides, "best")
	if len(m.Rows) != 2 || m.Rows[0].User.Login != "alice" || m.Rows[1].User.Login != "bob" {
		t.Fatalf("rows: %+v", m.Rows)
	}
	a := m.Rows[0]
	if c := a.Cells["t1"]; c.Display != 80 || c.Status != StatusPassed && c.Status != StatusPartial || c.LatestSubID != 11 {
		t.Errorf("alice t1: %+v", c)
	}
	if c := a.Cells["t2"]; c.Override == nil || c.Display != 33 || c.Computed != nil {
		t.Errorf("alice t2 override: %+v", c)
	}
	if a.Total != 113 {
		t.Errorf("alice total: %v", a.Total)
	}
	b := m.Rows[1]
	if c := b.Cells["t2"]; c.Status != "queued" || c.Display != 0 {
		t.Errorf("bob t2: %+v", c)
	}
	if c := b.Cells["t1"]; c.Status != "" || c.LatestSubID != 0 {
		t.Errorf("bob t1 empty cell: %+v", c)
	}
}

func TestLeaderboardRanking(t *testing.T) {
	m := Matrix{Rows: []Row{
		{User: store.User{Login: "a"}, Total: 50},
		{User: store.User{Login: "b"}, Total: 90},
		{User: store.User{Login: "c"}, Total: 50},
		{User: store.User{Login: "d"}, Total: 10},
	}}
	rows := Leaderboard(m)
	wantRanks := []int{1, 2, 2, 4}
	wantLogins := []string{"b", "a", "c", "d"}
	for i := range rows {
		if rows[i].Rank != wantRanks[i] || rows[i].Login != wantLogins[i] {
			t.Errorf("row %d: %+v, want rank %d login %s", i, rows[i], wantRanks[i], wantLogins[i])
		}
	}
	if a1, a2 := Alias("alice"), Alias("alice"); a1 != a2 {
		t.Errorf("alias must be stable: %s vs %s", a1, a2)
	}
	if Alias("alice") == Alias("bob") {
		t.Error("distinct logins should not collide on this input")
	}
}

func TestWriteCSV(t *testing.T) {
	m := Build(
		[]store.User{{ID: 1, Login: "alice", DisplayName: "Alice A", Role: "student"}},
		[]TaskCol{{ID: "t1", MaxScore: 100}, {ID: "t2", MaxScore: 50}},
		[]store.Submission{{ID: 1, UserID: 1, TaskID: "t1", Status: store.StatusDone, FinalScore: new(float64(48.5)), Counts: true}},
		nil, "best")
	var buf bytes.Buffer
	if err := WriteCSV(&buf, m); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if lines[0] != "login,display_name,t1,t2,total" {
		t.Errorf("header: %s", lines[0])
	}
	if lines[1] != "alice,Alice A,48.5,0,48.5" {
		t.Errorf("row: %s", lines[1])
	}
}
