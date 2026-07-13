package scoring

import (
	"testing"
	"time"
)

func TestRawScore(t *testing.T) {
	tests := []struct {
		name    string
		score   int
		results []CheckResult
		want    float64
	}{
		{
			name:  "gate pass, single scored check passes = full",
			score: 100,
			results: []CheckResult{
				{Name: "build", Required: true, Weight: 0, Passed: true},
				{Name: "basic", Weight: 60, Passed: true},
			},
			want: 100,
		},
		{
			name:  "gate pass, single scored check fails = zero (all-or-nothing)",
			score: 100,
			results: []CheckResult{
				{Name: "build", Required: true, Passed: true},
				{Name: "basic", Weight: 60, Passed: false},
			},
			want: 0,
		},
		{
			name:  "gate fails = zero regardless of scored checks",
			score: 100,
			results: []CheckResult{
				{Name: "build", Required: true, Passed: false},
				{Name: "basic", Weight: 60, Passed: true},
			},
			want: 0,
		},
		{
			name:  "partial credit by weight",
			score: 100,
			results: []CheckResult{
				{Name: "basic", Weight: 60, Passed: true},
				{Name: "advanced", Weight: 40, Passed: false},
			},
			want: 60,
		},
		{
			name:  "no gates, both pass",
			score: 200,
			results: []CheckResult{
				{Name: "a", Weight: 1, Passed: true},
				{Name: "b", Weight: 1, Passed: true},
			},
			want: 200,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RawScore(tc.score, tc.results); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func mustTime(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm
}

func TestPenaltyPercent(t *testing.T) {
	soft := mustTime("2026-09-24T00:00:00Z")
	pen := Penalty{Percent: 10, Per: 24 * time.Hour, MaxPercent: 50}

	tests := []struct {
		name string
		d    Deadline
		at   time.Time
		want float64
	}{
		{"no soft deadline", Deadline{Penalty: pen}, mustTime("2030-01-01T00:00:00Z"), 0},
		{"exactly at soft is on time", Deadline{Soft: &soft, Penalty: pen}, soft, 0},
		{"one second late = one interval", Deadline{Soft: &soft, Penalty: pen}, soft.Add(time.Second), 10},
		{"exactly 24h late = one interval", Deadline{Soft: &soft, Penalty: pen}, soft.Add(24 * time.Hour), 10},
		{"24h + 1s late = two intervals", Deadline{Soft: &soft, Penalty: pen}, soft.Add(24*time.Hour + time.Second), 20},
		{"capped at max_percent", Deadline{Soft: &soft, Penalty: pen}, soft.Add(100 * 24 * time.Hour), 50},
		{"max_percent 0 disables", Deadline{Soft: &soft, Penalty: Penalty{Percent: 10, Per: 24 * time.Hour, MaxPercent: 0}}, soft.Add(48 * time.Hour), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PenaltyPercent(tc.d, tc.at); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFinalScore(t *testing.T) {
	if got := FinalScore(100, 30); got != 70 {
		t.Fatalf("got %v, want 70", got)
	}
	if got := FinalScore(60, 0); got != 60 {
		t.Fatalf("got %v, want 60", got)
	}
}
