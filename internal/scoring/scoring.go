// Package scoring computes submission scores from check outcomes and applies
// deadline penalties (SPEC §4.3 weights, §9). All functions are pure; they take
// plain values so both the worker and `anygrade check` can reuse them.
package scoring

import (
	"math"
	"time"
)

// CheckResult is the outcome of one check needed for scoring.
type CheckResult struct {
	Name     string
	Required bool // gate: a failed gate forces the raw score to 0
	Weight   int
	Passed   bool
}

// Penalty is the late-submission penalty policy.
type Penalty struct {
	Percent    int
	Per        time.Duration
	MaxPercent int
}

// Deadline is the deadline configuration relevant to penalties.
type Deadline struct {
	Soft    *time.Time
	Hard    *time.Time
	Penalty Penalty
}

// RawScore computes the raw score in [0, taskScore]. A failed required (gate)
// check forces 0. Otherwise the score is taskScore weighted by the fraction of
// passed non-gate weight (SPEC §4.3).
func RawScore(taskScore int, results []CheckResult) float64 {
	// Gates first: any failed gate zeroes the submission.
	for _, c := range results {
		if c.Required && !c.Passed {
			return 0
		}
	}

	var totalW, passedW int
	scored := 0
	for _, c := range results {
		if c.Required {
			continue
		}
		scored++
		totalW += c.Weight
		if c.Passed {
			passedW += c.Weight
		}
	}

	// Valid metadata guarantees at least one non-gate check with positive
	// weight (validation rule 23). Fall back defensively to all-or-nothing so
	// the function stays total for relaxed/historical data.
	if totalW == 0 {
		if scored > 0 && passedW == scored {
			// unreachable for valid data; keep total.
			return float64(taskScore)
		}
		allPassed := scored > 0
		for _, c := range results {
			if !c.Required && !c.Passed {
				allPassed = false
			}
		}
		if allPassed {
			return float64(taskScore)
		}
		return 0
	}

	return float64(taskScore) * float64(passedW) / float64(totalW)
}

// PenaltyPercent computes the late penalty percentage in [0, MaxPercent] for a
// submission accepted at submittedAt. Submissions past the hard deadline are
// rejected upstream (not graded), so this assumes submittedAt <= hard.
func PenaltyPercent(d Deadline, submittedAt time.Time) float64 {
	if d.Soft == nil {
		return 0 // no soft deadline: never penalized
	}
	if !submittedAt.After(*d.Soft) {
		return 0 // on time (boundary inclusive: submittedAt == soft is on time)
	}
	if d.Penalty.Per <= 0 {
		return 0 // misconfigured; validation flags this
	}
	elapsed := submittedAt.Sub(*d.Soft)
	intervals := math.Ceil(float64(elapsed) / float64(d.Penalty.Per)) // each started interval counts
	pct := intervals * float64(d.Penalty.Percent)
	return math.Min(pct, float64(d.Penalty.MaxPercent))
}

// FinalScore applies a penalty percentage to a raw score.
func FinalScore(raw, penaltyPercent float64) float64 {
	return raw * (1 - penaltyPercent/100)
}
