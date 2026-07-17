package config

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/ekalinin/anygrade/internal/i18n"
)

const (
	courseFile = "course.yaml"
	taskFile   = "task.yaml"
)

// decodeStrict decodes YAML with unknown-field detection enabled, satisfying
// the SPEC §4.3 rule that unknown fields are errors.
func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

// LoadCourse reads and strictly decodes a course.yaml file. Decode problems
// (unknown fields, malformed scalars) are returned as diagnostics; a missing or
// unreadable file is returned as an error.
func LoadCourse(path string) (*Course, []Diagnostic, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	c, diags := decodeCourse(data, filepath.Base(path))
	return c, diags, nil
}

func decodeCourse(data []byte, display string) (*Course, []Diagnostic) {
	var c Course
	if err := decodeStrict(data, &c); err != nil {
		return &c, []Diagnostic{{Severity: SevError, File: display, Message: err.Error()}}
	}
	return &c, nil
}

// LoadTask reads and strictly decodes the task.yaml in dir. Dir is recorded on
// the returned Task for later solution-file checks.
func LoadTask(dir string) (*Task, []Diagnostic, error) {
	path := filepath.Join(dir, taskFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	t, diags := decodeTask(data, path, dir)
	return t, diags, nil
}

func decodeTask(data []byte, display, dir string) (*Task, []Diagnostic) {
	var t Task
	if err := decodeStrict(data, &t); err != nil {
		t.Dir = dir
		return &t, []Diagnostic{{Severity: SevError, File: display, Message: err.Error()}}
	}
	t.Dir = dir
	return &t, nil
}

// LoadAll loads course.yaml and every task.yaml under the configured tasks
// directory, merges defaults, and returns the resolved course together with any
// decode diagnostics. A missing course.yaml is a fatal error. Call Validate on
// the result to obtain the full validation diagnostics.
func LoadAll(repoDir string) (*Resolved, []Diagnostic, error) {
	var diags []Diagnostic

	coursePath := filepath.Join(repoDir, courseFile)
	data, err := os.ReadFile(coursePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("%s not found in %s", courseFile, repoDir)
		}
		return nil, nil, err
	}
	rawCourse, cDiags := decodeCourse(data, courseFile)
	diags = append(diags, cDiags...)

	tasksDir := rawCourse.TasksDir
	if tasksDir == "" {
		tasksDir = "tasks"
	}
	tasksRoot := filepath.Join(repoDir, tasksDir)

	var tasks []ResolvedTask
	walkErr := filepath.WalkDir(tasksRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable subtrees but keep going.
			if path == tasksRoot {
				return fs.SkipAll
			}
			return nil
		}
		if d.IsDir() || d.Name() != taskFile {
			return nil
		}
		dir := filepath.Dir(path)
		display, relErr := filepath.Rel(repoDir, path)
		if relErr != nil {
			display = path
		}
		rawTask, tDiags := decodeTask(mustRead(path), display, dir)
		diags = append(diags, tDiags...)

		rt := Resolve(rawCourse, rawTask)
		rt.file = display
		tasks = append(tasks, rt)
		return nil
	})
	// A non-existent tasks root is not fatal here; Validate reports "no tasks".
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, diags, walkErr
	}

	resolved := &Resolved{
		Course:    resolveCourse(rawCourse),
		Tasks:     tasks,
		rawCourse: rawCourse,
	}
	return resolved, diags, nil
}

// mustRead reads a file whose existence was just established by WalkDir; a read
// error at this point is an unexpected I/O fault and yields empty content,
// which decodeStrict then reports as a diagnostic.
func mustRead(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

func resolveCourse(c *Course) ResolvedCourse {
	tasksDir := c.TasksDir
	if tasksDir == "" {
		tasksDir = "tasks"
	}
	policy := c.Scoring.Policy
	if policy == "" {
		policy = "best"
	}
	lang := c.Language
	if lang == "" {
		lang = i18n.Default
	}
	return ResolvedCourse{
		Name:          c.Name,
		TasksDir:      tasksDir,
		Language:      lang,
		Registration:  c.Registration,
		Leaderboard:   c.Leaderboard,
		ScoringPolicy: policy,
	}
}
