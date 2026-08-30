package config

import "time"

// Resolved is the fully loaded and merged course: defaults are already applied
// to every task. This is what the rest of the application consumes.
type Resolved struct {
	Course ResolvedCourse
	Tasks  []ResolvedTask

	// rawCourse retains the raw course for validation checks that need to know
	// whether a field was explicitly set. Unexported; not part of the API.
	rawCourse *Course
}

// ResolvedCourse is the course-level configuration with built-in defaults
// applied (e.g. tasks_dir, scoring policy).
type ResolvedCourse struct {
	Name          string
	TasksDir      string
	Language      string
	Registration  Registration
	Leaderboard   Leaderboard
	ScoringPolicy string
	// MaxPushSize caps one git push in bytes (SPEC §13); always > 0.
	MaxPushSize int64
	// Timezone is the location the UI renders timestamps in (SPEC §13);
	// never nil - an unset or unloadable name resolves to UTC, and Validate
	// reports the latter as an error.
	Timezone *time.Location
}

// ResolvedTask is a task with all runner/limits/deadline defaults merged in.
type ResolvedTask struct {
	ID            string
	Name          string
	Score         int
	Dir           string
	SolutionFiles []string
	Runner        ResolvedRunner
	Limits        ResolvedLimits
	Deadline      ResolvedDeadline
	Hidden        *HiddenTests
	Checks        []Check
	Workspace     ResolvedWorkspace

	// raw, root and file support validation (explicit-set detection, resolving
	// repo-relative paths, diagnostics).
	raw  *Task
	root string // course repo root, exactly as it was passed to LoadAll
	file string // repo-relative path to task.yaml
}

// ResolvedWorkspace is the merged set of extra paths exported into the check
// workspace alongside the task directory, plus the size bounds of the
// student-controlled overlay.
type ResolvedWorkspace struct {
	Include      []string
	MaxFileSize  int64
	MaxTotalSize int64
}

// ResolvedRunner is a concrete runner configuration (all defaults applied).
type ResolvedRunner struct {
	Type       string
	Image      string
	Timeout    time.Duration
	Memory     int64
	CPUs       float64
	Network    string
	LogExcerpt int64
	LogMax     int64
}

// ResolvedLimits is a concrete attempt/cooldown configuration.
type ResolvedLimits struct {
	MaxAttempts int
	Cooldown    time.Duration
}

// ResolvedDeadline is a concrete deadline configuration.
type ResolvedDeadline struct {
	Soft    *time.Time
	Hard    *time.Time
	Penalty ResolvedPenalty
}

// ResolvedPenalty is a concrete late-penalty configuration.
type ResolvedPenalty struct {
	Percent    int
	Per        time.Duration
	MaxPercent int
}
