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
		{
			// The point of a parser: one check, 19 of 20 cases, and a score
			// that is neither 0 nor 100 (SPEC §4.3).
			name:  "parsed cases split one check's weight",
			score: 100,
			results: []CheckResult{
				{Name: "unit", Weight: 100, Passed: false, PassedCases: 19, ScoredCases: 20},
			},
			want: 95,
		},
		{
			name:  "a parsed check's proportion is of its own weight only",
			score: 100,
			results: []CheckResult{
				{Name: "basic", Weight: 60, Passed: false, PassedCases: 1, ScoredCases: 2},
				{Name: "advanced", Weight: 40, Passed: true},
			},
			want: 70,
		},
		{
			// An unreadable report leaves both counters at 0, which is exactly
			// the all-or-nothing the check had before the parser was added.
			name:  "no tally falls back to the exit code",
			score: 100,
			results: []CheckResult{
				{Name: "unit", Weight: 100, Passed: true, PassedCases: 0, ScoredCases: 0},
			},
			want: 100,
		},
		{
			// Skipped cases count for neither side, so a report of nothing but
			// skips has no proportion and the exit code decides.
			name:  "all cases skipped falls back to the exit code",
			score: 100,
			results: []CheckResult{
				{Name: "unit", Weight: 100, Passed: false, ScoredCases: 0},
			},
			want: 0,
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

// TestRawScoreGateIsTheExitCode: a parser changes what a check is worth, never
// whether it gates. A required check that passed 19 of 20 cases and exited
// non-zero still zeroes the submission - "partially gated" is not a thing, and
// the report a parser reads is written beside the student's own code, so a
// gate that believed it would be a gate the student holds (SPEC §4.3, §14).
func TestRawScoreGateIsTheExitCode(t *testing.T) {
	results := []CheckResult{
		{Name: "gate", Required: true, Passed: false, PassedCases: 19, ScoredCases: 20},
		{Name: "basic", Weight: 100, Passed: true},
	}
	if got := RawScore(100, results); got != 0 {
		t.Errorf("a failed gate scores 0 whatever its cases say, got %v", got)
	}
	// And a gate that exited 0 admits the submission even when its report is
	// unhappy: the gate is the command's verdict, in both directions.
	results[0].Passed = true
	if got := RawScore(100, results); got != 100 {
		t.Errorf("a passed gate must not cost anything, got %v", got)
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
