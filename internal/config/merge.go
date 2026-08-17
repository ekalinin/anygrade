package config

import (
	"path/filepath"
	"time"
)

// Built-in fallback defaults, used when course.yaml omits a defaults field
// entirely. These are the base layer of the merge (SPEC §4.2 example values).
// DefaultLogExcerpt is the built-in per-check log excerpt size (SPEC §13:
// "truncated to a configurable excerpt, default 64 KB per check"). It lives
// here rather than in runner because runner imports config, not the reverse.
const DefaultLogExcerpt = 64 << 10

// DefaultMaxPushSize is the built-in `limits.max_push_size` (SPEC §13:
// "max_push_size (default 50 MB) guards against giant blobs").
const DefaultMaxPushSize = 50 << 20

// DefaultLogMax bounds the full on-disk log of one check, DefaultOverlayFile
// and DefaultOverlayTotal the decompressed size of the student overlay. All
// three exist because the data they bound is produced by untrusted code.
const (
	DefaultLogMax       = 10 << 20
	DefaultOverlayFile  = 10 << 20
	DefaultOverlayTotal = 64 << 20
)

func builtinRunner() ResolvedRunner {
	return ResolvedRunner{
		Type:       "docker",
		Image:      "",
		Timeout:    5 * time.Minute,
		Memory:     512 << 20, // 512m
		CPUs:       1,
		Network:    "none",
		LogExcerpt: DefaultLogExcerpt,
		LogMax:     DefaultLogMax,
	}
}

func builtinLimits() ResolvedLimits {
	return ResolvedLimits{MaxAttempts: 0, Cooldown: 0}
}

func builtinPenalty() ResolvedPenalty {
	return ResolvedPenalty{Percent: 0, Per: 24 * time.Hour, MaxPercent: 0}
}

// Resolve merges defaults into a single task: builtin → course defaults → task.
// Each stage overlays only the fields it explicitly sets, so unset fields
// inherit from the layer below (deep, field-by-field merge, SPEC §4.2).
func Resolve(c *Course, t *Task) ResolvedTask {
	courseRunner := mergeRunner(builtinRunner(), c.Defaults.Runner)
	courseLimits := mergeLimits(builtinLimits(), c.Defaults.Limits)
	coursePenalty := mergePenalty(builtinPenalty(), c.Defaults.Deadline.Penalty)

	id := t.ID
	if id == "" {
		id = filepath.Base(t.Dir)
	}

	return ResolvedTask{
		ID:            id,
		Name:          t.Name,
		Score:         t.Score,
		Dir:           t.Dir,
		SolutionFiles: t.SolutionFiles,
		Runner:        mergeRunner(courseRunner, t.Runner),
		Limits:        mergeLimits(courseLimits, t.Limits),
		Deadline: ResolvedDeadline{
			Soft:    tsPtr(t.Deadline.Soft),
			Hard:    tsPtr(t.Deadline.Hard),
			Penalty: mergePenalty(coursePenalty, t.Deadline.Penalty),
		},
		Hidden:    t.HiddenTests,
		Checks:    t.Checks,
		Workspace: mergeWorkspace(c.Defaults.Workspace, t.Workspace),
		raw:       t,
	}
}

// mergeWorkspace unions course-level and task-level include lists (course
// first, order stable, paths cleaned, duplicates removed). The overlay size
// bounds are overlaid field by field like every other spec.
func mergeWorkspace(course, task WorkspaceSpec) ResolvedWorkspace {
	seen := map[string]bool{}
	var include []string
	for _, p := range append(append([]string{}, course.Include...), task.Include...) {
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		include = append(include, clean)
	}
	w := ResolvedWorkspace{
		Include:      include,
		MaxFileSize:  DefaultOverlayFile,
		MaxTotalSize: DefaultOverlayTotal,
	}
	for _, over := range []WorkspaceSpec{course, task} {
		if over.MaxFileSize != nil {
			w.MaxFileSize = over.MaxFileSize.Bytes()
		}
		if over.MaxTotalSize != nil {
			w.MaxTotalSize = over.MaxTotalSize.Bytes()
		}
	}
	return w
}

// mergeRunner overlays the non-nil fields of over onto base.
func mergeRunner(base ResolvedRunner, over RunnerSpec) ResolvedRunner {
	if over.Type != nil {
		base.Type = *over.Type
	}
	if over.Image != nil {
		base.Image = *over.Image
	}
	if over.Timeout != nil {
		base.Timeout = over.Timeout.Std()
	}
	if over.Memory != nil {
		base.Memory = over.Memory.Bytes()
	}
	if over.CPUs != nil {
		base.CPUs = *over.CPUs
	}
	if over.Network != nil {
		base.Network = *over.Network
	}
	if over.LogExcerpt != nil {
		base.LogExcerpt = over.LogExcerpt.Bytes()
	}
	if over.LogMax != nil {
		base.LogMax = over.LogMax.Bytes()
	}
	return base
}

// mergeLimits overlays the non-nil fields of over onto base.
func mergeLimits(base ResolvedLimits, over Limits) ResolvedLimits {
	if over.MaxAttempts != nil {
		base.MaxAttempts = *over.MaxAttempts
	}
	if over.Cooldown != nil {
		base.Cooldown = over.Cooldown.Std()
	}
	return base
}

// mergePenalty overlays the non-nil fields of over onto base.
func mergePenalty(base ResolvedPenalty, over Penalty) ResolvedPenalty {
	if over.Percent != nil {
		base.Percent = *over.Percent
	}
	if over.Per != nil {
		base.Per = over.Per.Std()
	}
	if over.MaxPercent != nil {
		base.MaxPercent = *over.MaxPercent
	}
	return base
}

func tsPtr(t *Timestamp) *time.Time {
	if t == nil {
		return nil
	}
	std := t.Std()
	return &std
}
