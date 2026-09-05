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
	// PassedCases and ScoredCases are the per-test-case tally of a check that
	// declared a `parser:` and whose report was read (SPEC §4.3). Both are 0
	// otherwise - no parser, an unreadable report, a report of nothing but
	// skips - and the check is then worth all of its weight or none of it, by
	// exit code, exactly as every check was before parsers existed.
	PassedCases int
	ScoredCases int
}

// fraction is how much of its weight a check earned: the parsed proportion
// when there is one, the 0-or-1 of the exit code otherwise.
func (c CheckResult) fraction() float64 {
	if c.ScoredCases > 0 {
		return float64(min(c.PassedCases, c.ScoredCases)) / float64(c.ScoredCases)
	}
	if c.Passed {
		return 1
	}
	return 0
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
// non-gate weight the checks earned (SPEC §4.3).
//
// Gates are decided by the exit code and by nothing else, parser or no parser.
// "Partially gated" is not a thing, and the output a parser reads is produced
// in the same workspace as the student's own code (SPEC §14): a parser may
// therefore change what a check is worth, never whether it blocks.
func RawScore(taskScore int, results []CheckResult) float64 {
	// Gates first: any failed gate zeroes the submission.
	for _, c := range results {
		if c.Required && !c.Passed {
			return 0
		}
	}

	var totalW int
	var earnedW float64
	scored := 0
	for _, c := range results {
		if c.Required {
			continue
		}
		scored++
		totalW += c.Weight
		earnedW += float64(c.Weight) * c.fraction()
	}

	// Valid metadata guarantees at least one non-gate check with positive
	// weight (validation rule 23). Fall back defensively to all-or-nothing so
	// the function stays total for relaxed/historical data.
	if totalW == 0 {
		if scored > 0 && earnedW == float64(scored) {
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

	return float64(taskScore) * earnedW / float64(totalW)
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
