package config

// Course is the raw, yaml-facing representation of course.yaml (SPEC §4.2).
// Fields that participate in per-task defaults inheritance are held as pointers
// inside the nested spec types so that "unset" is distinguishable from "set to
// zero"; course-level-only scalars are plain values.
type Course struct {
	Name         string       `yaml:"name"`
	TasksDir     string       `yaml:"tasks_dir"`
	Registration Registration `yaml:"registration"`
	Leaderboard  Leaderboard  `yaml:"leaderboard"`
	Scoring      Scoring      `yaml:"scoring"`
	Defaults     Defaults     `yaml:"defaults"`
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
	Runner   RunnerSpec       `yaml:"runner"`
	Limits   Limits           `yaml:"limits"`
	Deadline DeadlineDefaults `yaml:"deadline"`
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
