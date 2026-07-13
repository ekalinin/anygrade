package config

import (
	"testing"
	"time"
)

// TestRunnerInheritsUnsetFields verifies deep, field-by-field merge: a task that
// overrides only runner.image inherits type/timeout/memory/cpus/network from the
// course defaults (SPEC §4.2).
func TestRunnerInheritsUnsetFields(t *testing.T) {
	course := &Course{
		Defaults: Defaults{
			Runner: RunnerSpec{
				Type:    new("docker"),
				Image:   new("golang:1.24"),
				Timeout: new(Duration(5 * time.Minute)),
				Memory:  new(ByteSize(512 << 20)),
				CPUs:    new(1.0),
				Network: new("none"),
			},
			Limits: Limits{MaxAttempts: new(0), Cooldown: new(Duration(0))},
		},
	}
	task := &Task{
		Dir:    "/tmp/tasks/01-intro",
		Runner: RunnerSpec{Image: new("golang:1.24-alpine")}, // only image set
	}

	r := Resolve(course, task)

	if r.Runner.Image != "golang:1.24-alpine" {
		t.Errorf("image: got %q, want override golang:1.24-alpine", r.Runner.Image)
	}
	if r.Runner.Type != "docker" {
		t.Errorf("type: got %q, want inherited docker", r.Runner.Type)
	}
	if r.Runner.Timeout != 5*time.Minute {
		t.Errorf("timeout: got %v, want inherited 5m", r.Runner.Timeout)
	}
	if r.Runner.Memory != 512<<20 {
		t.Errorf("memory: got %d, want inherited 512m", r.Runner.Memory)
	}
	if r.Runner.Network != "none" {
		t.Errorf("network: got %q, want inherited none", r.Runner.Network)
	}
	if r.ID != "01-intro" {
		t.Errorf("id: got %q, want dir-name default 01-intro", r.ID)
	}
}

// TestBuiltinDefaultsWhenCourseOmits verifies the builtin base layer applies
// when course.yaml specifies no defaults at all.
func TestBuiltinDefaultsWhenCourseOmits(t *testing.T) {
	r := Resolve(&Course{}, &Task{Dir: "/tmp/tasks/x", ID: "x"})
	if r.Runner.Type != "docker" || r.Runner.Timeout != 5*time.Minute || r.Runner.Memory != 512<<20 {
		t.Errorf("builtin runner not applied: %+v", r.Runner)
	}
	if r.Deadline.Penalty.Per != 24*time.Hour {
		t.Errorf("builtin penalty.per: got %v, want 24h", r.Deadline.Penalty.Per)
	}
}

// TestCourseDefaultsOverrideBuiltin verifies course defaults win over builtin,
// and a task with no override inherits the course default.
func TestCourseDefaultsOverrideBuiltin(t *testing.T) {
	course := &Course{
		Defaults: Defaults{Runner: RunnerSpec{Image: new("python:3.13")}},
	}
	r := Resolve(course, &Task{Dir: "/tmp/tasks/y", ID: "y"})
	if r.Runner.Image != "python:3.13" {
		t.Errorf("image: got %q, want course default python:3.13", r.Runner.Image)
	}
	if r.Runner.Type != "docker" { // still builtin
		t.Errorf("type: got %q, want builtin docker", r.Runner.Type)
	}
}
