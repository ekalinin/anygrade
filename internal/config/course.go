package config

// Course is the raw, yaml-facing representation of course.yaml (SPEC §4.2).
// Fields that participate in per-task defaults inheritance are held as pointers
// inside the nested spec types so that "unset" is distinguishable from "set to
// zero"; course-level-only scalars are plain values.
type Course struct {
	Name         string       `yaml:"name"`
	TasksDir     string       `yaml:"tasks_dir"`
	Language     string       `yaml:"language"` // web UI language (en | ru); empty defaults to en
	Timezone     string       `yaml:"timezone"` // IANA name the UI renders times in; empty defaults to UTC
	Registration Registration `yaml:"registration"`
	Leaderboard  Leaderboard  `yaml:"leaderboard"`
	Scoring      Scoring      `yaml:"scoring"`
	Limits       CourseLimits `yaml:"limits"`
	Defaults     Defaults     `yaml:"defaults"`
}

// CourseLimits are the instance-wide limits (SPEC §13). They sit next to
// `defaults:` rather than inside it because they describe the git server, not
// a task, and therefore never take part in the per-task defaults inheritance
// that `defaults.limits` drives.
type CourseLimits struct {
	// MaxPushSize caps one git push in bytes; unset = DefaultMaxPushSize.
	MaxPushSize *ByteSize `yaml:"max_push_size"`
}

// Registration configures how students get accounts (SPEC §8).
type Registration struct {
	Mode       string `yaml:"mode"`        // invite | open
	CourseCode string `yaml:"course_code"` // required iff mode == open
}

// Leaderboard configures the optional ranked score view.
type Leaderboard struct {
	Enabled   bool `yaml:"enabled"`
	Anonymize bool `yaml:"anonymize"`
}

// Scoring selects which submission counts per task.
type Scoring struct {
	Policy string `yaml:"policy"` // best | latest (empty defaults to best)
}

// Defaults are inherited by every task and overridable per task (SPEC §4.2).
type Defaults struct {
	Runner    RunnerSpec       `yaml:"runner"`
	Limits    Limits           `yaml:"limits"`
	Deadline  DeadlineDefaults `yaml:"deadline"`
	Workspace WorkspaceSpec    `yaml:"workspace"`
}

// WorkspaceSpec lists extra repo-relative paths exported into the check
// workspace in addition to the task directory (e.g. a course-root go.mod),
// and bounds the student-controlled part of the workspace.
// Course-level and task-level lists are unioned.
type WorkspaceSpec struct {
	Include []string `yaml:"include"`
	// MaxFileSize / MaxTotalSize cap one overlaid solution file and the whole
	// overlay after decompression: the push limit only bounds the compressed
	// pack, so a highly compressible blob passes it and expands afterwards.
	MaxFileSize  *ByteSize `yaml:"max_file_size"`
	MaxTotalSize *ByteSize `yaml:"max_total_size"`
}

// DeadlineDefaults holds only the penalty policy; soft/hard timestamps are
// task-specific and never inherited.
type DeadlineDefaults struct {
	Penalty Penalty `yaml:"penalty"`
}

// RunnerSpec is a mergeable runner configuration. Every field is a pointer so
// an unset field inherits from the layer below (builtin → course → task).
type RunnerSpec struct {
	Type    *string   `yaml:"type"`    // docker | local
	Image   *string   `yaml:"image"`   //
	Timeout *Duration `yaml:"timeout"` //
	Memory  *ByteSize `yaml:"memory"`  //
	CPUs    *float64  `yaml:"cpus"`    //
	Network *string   `yaml:"network"` // none | bridge | host
	// LogExcerpt is the per-check log tail kept in the DB and shown in the UI
	// (SPEC §13); the full log always stays on disk.
	LogExcerpt *ByteSize `yaml:"log_excerpt"`
	// LogMax caps the full on-disk log of one check. Check output is untrusted,
	// so it must not be able to fill the host disk (SPEC §14).
	LogMax *ByteSize `yaml:"log_max"`
}

// Limits is a mergeable attempt/cooldown configuration. MaxAttempts is a
// pointer because 0 (= unlimited) is a meaningful value distinct from "inherit".
type Limits struct {
	MaxAttempts *int      `yaml:"max_attempts"` // 0 = unlimited
	Cooldown    *Duration `yaml:"cooldown"`
}

// Penalty is a mergeable late-submission penalty configuration.
type Penalty struct {
	Percent    *int      `yaml:"percent"`     // per interval; 0 is meaningful
	Per        *Duration `yaml:"per"`         //
	MaxPercent *int      `yaml:"max_percent"` // cap; 0 is meaningful
}
