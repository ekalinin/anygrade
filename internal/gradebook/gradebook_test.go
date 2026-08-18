package gradebook

import (
	"bytes"
	"encoding/csv"
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
		name       string
		history    []store.Submission
		overridden bool
		want       string
	}{
		{"empty", nil, false, StatusNotStarted},
		{"overridden without submissions", nil, true, StatusOverridden},
		{"an override does not mask a real outcome",
			[]store.Submission{sub(store.StatusDone, new(float64(0)), true)}, true, StatusFailed},
		{"queued", []store.Submission{sub(store.StatusQueued, nil, true)}, false, "queued"},
		{"running", []store.Submission{sub(store.StatusRunning, nil, true)}, false, "running"},
		{"passed", []store.Submission{sub(store.StatusDone, new(float64(100)), true)}, false, StatusPassed},
		{"partial", []store.Submission{sub(store.StatusDone, new(float64(60)), true)}, false, StatusPartial},
		{"failed", []store.Submission{sub(store.StatusDone, new(float64(0)), true)}, false, StatusFailed},
		{"rejected", []store.Submission{sub(store.StatusRejectedDeadline, nil, true)}, false, StatusRejected},
		{"latest wins", []store.Submission{
			sub(store.StatusDone, new(float64(100)), true),
			sub(store.StatusQueued, nil, true),
		}, false, "queued"},
		{"retrying", []store.Submission{{Status: store.StatusInfraError, RetryAt: &retryAt}}, false, StatusRetrying},
		{"terminal infra", []store.Submission{{Status: store.StatusInfraError}}, false, StatusError},
		{"canceled", []store.Submission{{Status: store.StatusInfraError, CanceledAt: &canceledAt}}, false, StatusCanceled},
	}
	for _, tc := range tests {
		if got := DeriveStatus(tc.history, 100, tc.overridden); got != tc.want {
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
	// An override with no submissions behind it: the score counts toward the
	// total, so the cell must carry a status the matrix draws instead of a dash.
	if c := a.Cells["t2"]; c.Override == nil || c.Display != 33 || c.Computed != nil ||
		c.Status != StatusOverridden {
		t.Errorf("alice t2 override: %+v", c)
	}
	if a.Total != 113 {
		t.Errorf("alice total: %v", a.Total)
	}
	assertTotalIsVisible(t, a)
	assertTotalIsVisible(t, m.Rows[1])
	b := m.Rows[1]
	if c := b.Cells["t2"]; c.Status != "queued" || c.Display != 0 {
		t.Errorf("bob t2: %+v", c)
	}
	if c := b.Cells["t1"]; c.Status != "" || c.LatestSubID != 0 {
		t.Errorf("bob t1 empty cell: %+v", c)
	}
}

// assertTotalIsVisible: the matrix draws a dash for a cell with an empty
// status and the score for every other one, so the row total has to equal the
// sum of the cells that are not blank. A cell that contributes to the total
// while showing a dash makes the row not add up.
func assertTotalIsVisible(t *testing.T, row Row) {
	t.Helper()
	visible := 0.0
	for id, c := range row.Cells {
		if c.Status == "" {
			if c.Display != 0 {
				t.Errorf("%s %s: a blank cell contributes %v to the total",
					row.User.Login, id, c.Display)
			}
			continue
		}
		visible += c.Display
	}
	if visible != row.Total {
		t.Errorf("%s: visible cells sum to %v, row total is %v", row.User.Login, visible, row.Total)
	}
}

func TestLeaderboardRanking(t *testing.T) {
	m := Matrix{Rows: []Row{
		{User: store.User{Login: "a"}, Total: 50},
		{User: store.User{Login: "b"}, Total: 90},
		{User: store.User{Login: "c"}, Total: 50},
		{User: store.User{Login: "d"}, Total: 10},
	}}
	a := NewAliaser([]byte("instance secret"))
	rows := Leaderboard(m, a)
	wantRanks := []int{1, 2, 2, 4}
	wantLogins := []string{"b", "a", "c", "d"}
	for i := range rows {
		if rows[i].Rank != wantRanks[i] || rows[i].Login != wantLogins[i] {
			t.Errorf("row %d: %+v, want rank %d login %s", i, rows[i], wantRanks[i], wantLogins[i])
		}
	}
	if a1, a2 := a.Alias("alice"), a.Alias("alice"); a1 != a2 {
		t.Errorf("alias must be stable: %s vs %s", a1, a2)
	}
	if a.Alias("alice") == a.Alias("bob") {
		t.Error("distinct logins should not collide on this input")
	}
}

// TestAliasIsKeyed: an alias must not be reproducible from the algorithm
// alone. The alphabet is small and the roster is guessable, so an unkeyed hash
// de-anonymizes the whole board by offline brute force - the opposite of what
// SPEC §10 asks anonymization for.
func TestAliasIsKeyed(t *testing.T) {
	one := NewAliaser([]byte("secret one"))
	two := NewAliaser([]byte("secret two"))

	differs := false
	for _, login := range []string{"alice", "bob", "carol", "dave", "erin", "frank"} {
		if one.Alias(login) != two.Alias(login) {
			differs = true
		}
		// Same secret, same aliases: they must survive a restart of one
		// instance, which is what the persisted key file buys.
		if got := NewAliaser([]byte("secret one")).Alias(login); got != one.Alias(login) {
			t.Errorf("alias of %q is not stable for one secret: %q vs %q", login, got, one.Alias(login))
		}
	}
	if !differs {
		t.Error("two instances with different secrets produced identical aliases")
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

// TestWriteCSVNeutralizesFormulas: a display name comes straight out of open
// registration, so every spreadsheet formula prefix must be defused before the
// teacher opens the export.
func TestWriteCSVNeutralizesFormulas(t *testing.T) {
	for _, name := range []string{
		`=cmd|' /c calc'!A1`,
		`+1+1`,
		`-1+1`,
		`@SUM(1:2)`,
	} {
		m := Build(
			[]store.User{{ID: 1, Login: "alice", DisplayName: name, Role: "student"}},
			[]TaskCol{{ID: "t1", MaxScore: 100}}, nil, nil, "best")
		var buf bytes.Buffer
		if err := WriteCSV(&buf, m); err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(&buf).ReadAll()
		if err != nil {
			t.Fatalf("%q: reparse: %v", name, err)
		}
		got := rows[1][1]
		if want := "'" + name; got != want {
			t.Errorf("display name %q exported as %q, want %q", name, got, want)
		}
	}
	// Scores keep their plain shape: they are never user-controlled.
	m := Build(
		[]store.User{{ID: 1, Login: "alice", DisplayName: "Alice", Role: "student"}},
		[]TaskCol{{ID: "t1", MaxScore: 100}}, nil, nil, "best")
	var buf bytes.Buffer
	if err := WriteCSV(&buf, m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "alice,Alice,0,0") {
		t.Errorf("untouched row was rewritten: %s", buf.String())
	}
}
