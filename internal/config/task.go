package config

// Task is the raw, yaml-facing representation of task.yaml (SPEC §4.3).
// Runner and Limits reuse the mergeable spec types from course.go so their
// fields inherit from course defaults field by field.
type Task struct {
	ID            string        `yaml:"id"`   // optional; defaults to dir name
	Name          string        `yaml:"name"` //
	Score         int           `yaml:"score"`
	SolutionFiles []string      `yaml:"solution_files"`
	Deadline      TaskDeadline  `yaml:"deadline"`
	Limits        Limits        `yaml:"limits"`
	Runner        RunnerSpec    `yaml:"runner"`
	HiddenTests   *HiddenTests  `yaml:"hidden_tests"` // whole block optional
	Checks        []Check       `yaml:"checks"`
	Workspace     WorkspaceSpec `yaml:"workspace"`

	// Dir is the absolute path of the task directory, filled by the loader.
	// Not a yaml field.
	Dir string `yaml:"-"`
}

// TaskDeadline holds the task's optional soft/hard timestamps and its penalty
// policy (which inherits course defaults).
type TaskDeadline struct {
	Soft    *Timestamp `yaml:"soft"`
	Hard    *Timestamp `yaml:"hard"`
	Penalty Penalty    `yaml:"penalty"`
}

// HiddenTests points at a private source of extra tests (SPEC §4.3). It is
// present or absent as a whole; it does not participate in field inheritance.
type HiddenTests struct {
	Source string `yaml:"source"` // git | local
	URL    string `yaml:"url"`    // required iff source == git
	Ref    string `yaml:"ref"`
	Path   string `yaml:"path"` // subdir (git) or absolute path (local)
}

// Check is one named command with a weight; a submission runs the task's list
// of checks in order (SPEC §4.3).
type Check struct {
	Name     string `yaml:"name"`
	Required bool   `yaml:"required"` // gate: failure stops the run, scores 0
	Weight   int    `yaml:"weight"`
	Run      string `yaml:"run"`
}
