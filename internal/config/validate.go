package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/i18n"
)

// Severity classifies a Diagnostic. Only SevError makes validation (and server
// startup) fail; warnings are informational.
type Severity int

const (
	SevError Severity = iota
	SevWarning
)

func (s Severity) String() string {
	if s == SevWarning {
		return "WARNING"
	}
	return "ERROR"
}

// Diagnostic is one validation finding, tied to a file and (optionally) a
// dotted field path.
type Diagnostic struct {
	Severity Severity
	File     string // e.g. "course.yaml" or "tasks/01-intro/task.yaml"
	Field    string // dotted path, e.g. "checks[1].weight" (may be empty)
	Message  string
}

// String renders a human-readable one-line diagnostic.
func (d Diagnostic) String() string {
	loc := d.File
	if d.Field != "" {
		loc = fmt.Sprintf("%s [%s]", d.File, d.Field)
	}
	return fmt.Sprintf("%s %s: %s", d.Severity, loc, d.Message)
}

// HasErrors reports whether any diagnostic is a SevError.
func HasErrors(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == SevError {
			return true
		}
	}
	return false
}

// maxSaneLogExcerpt is the point above which runner.log_excerpt only earns a
// warning, not an error: bigger excerpts work, they just cost memory per
// running check and space in every submission row.
const maxSaneLogExcerpt = 1 << 20

var (
	validRunnerTypes  = map[string]bool{"docker": true, "local": true}
	validNetworks     = map[string]bool{"none": true, "bridge": true, "host": true}
	validRegModes     = map[string]bool{"invite": true, "open": true}
	validScorePolicy  = map[string]bool{"best": true, "latest": true}
	validHiddenSource = map[string]bool{"git": true, "local": true}
)

// Validate applies the full metadata ruleset (SPEC §4.3, §13) to a resolved
// course. Load-time diagnostics (unknown fields, malformed scalars) come from
// LoadAll separately; callers should report both sets together.
func Validate(r *Resolved) []Diagnostic {
	var diags []Diagnostic
	add := func(sev Severity, file, field, format string, args ...any) {
		diags = append(diags, Diagnostic{sev, file, field, fmt.Sprintf(format, args...)})
	}

	c := r.Course
	// Course-level rules (3-7).
	if c.Name == "" {
		add(SevError, courseFile, "name", "course name is required")
	}
	if !validRegModes[c.Registration.Mode] {
		add(SevError, courseFile, "registration.mode", "must be one of invite|open, got %q", c.Registration.Mode)
	}
	if c.Registration.Mode == "open" && c.Registration.CourseCode == "" {
		add(SevError, courseFile, "registration.course_code", "required when registration.mode is open")
	}
	if p := r.rawCourse.Scoring.Policy; p != "" && !validScorePolicy[p] {
		add(SevError, courseFile, "scoring.policy", "must be one of best|latest, got %q", p)
	}
	if l := r.rawCourse.Language; l != "" && !i18n.Supported(l) {
		add(SevError, courseFile, "language", "must be one of %s, got %q", strings.Join(i18n.Locales(), "|"), l)
	}
	if tz := r.rawCourse.Timezone; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			add(SevError, courseFile, "timezone", "must be an IANA location name (e.g. Europe/Berlin), got %q", tz)
		}
	}
	if len(r.Tasks) == 0 {
		add(SevError, courseFile, "tasks_dir", "no task.yaml found under %q", c.TasksDir)
	}

	seenIDs := map[string]string{} // id -> first file that used it
	for i := range r.Tasks {
		t := &r.Tasks[i]
		validateTask(t, add)

		if prev, dup := seenIDs[t.ID]; dup {
			add(SevError, t.file, "id", "duplicate task id %q (also in %s)", t.ID, prev)
		} else {
			seenIDs[t.ID] = t.file
		}
	}
	return diags
}

func validateTask(t *ResolvedTask, add func(Severity, string, string, string, ...any)) {
	f := t.file

	// Runner (8-11).
	if !validRunnerTypes[t.Runner.Type] {
		add(SevError, f, "runner.type", "must be one of docker|local, got %q", t.Runner.Type)
	}
	if !validNetworks[t.Runner.Network] {
		add(SevError, f, "runner.network", "must be one of none|bridge|host, got %q", t.Runner.Network)
	}
	if t.Runner.Type == "docker" && t.Runner.Image == "" {
		add(SevError, f, "runner.image", "image is required for docker runner")
	}
	if t.Runner.Timeout <= 0 {
		add(SevError, f, "runner.timeout", "must be > 0")
	}
	if t.Runner.Memory <= 0 {
		add(SevError, f, "runner.memory", "must be > 0")
	}
	if t.Runner.CPUs <= 0 {
		add(SevError, f, "runner.cpus", "must be > 0")
	}
	if t.Runner.LogExcerpt <= 0 {
		add(SevError, f, "runner.log_excerpt", "must be > 0")
	} else if t.Runner.LogExcerpt > maxSaneLogExcerpt {
		// The excerpt is buffered in memory per running check and then stored
		// in the DB row; the full log is on disk either way.
		add(SevWarning, f, "runner.log_excerpt", "%d bytes is kept in memory per check and stored in the database", t.Runner.LogExcerpt)
	}
	// Memory/cpu limits are docker-only (SPEC §14); warn when a local-runner
	// task sets them explicitly so they don't look enforced.
	if t.Runner.Type == "local" && t.raw != nil {
		if t.raw.Runner.Memory != nil {
			add(SevWarning, f, "runner.memory", "memory limit is not enforced by the local runner")
		}
		if t.raw.Runner.CPUs != nil {
			add(SevWarning, f, "runner.cpus", "cpu limit is not enforced by the local runner")
		}
	}

	// Identity (13-14).
	if strings.ContainsAny(t.ID, "/ \t\n") || strings.Contains(t.ID, "..") {
		add(SevError, f, "id", "task id %q must not contain '/', '..', or whitespace", t.ID)
	}
	if t.Score <= 0 {
		add(SevError, f, "score", "must be > 0")
	}

	// Solution files (15-16).
	if len(t.SolutionFiles) == 0 {
		add(SevError, f, "solution_files", "at least one solution file is required")
	}
	for i, sf := range t.SolutionFiles {
		field := fmt.Sprintf("solution_files[%d]", i)
		if filepath.IsAbs(sf) {
			add(SevError, f, field, "%q must be a relative path", sf)
			continue
		}
		clean := filepath.Clean(sf)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			add(SevError, f, field, "%q must not escape the task directory", sf)
			continue
		}
		if _, err := os.Stat(filepath.Join(t.Dir, clean)); err != nil {
			add(SevError, f, field, "listed file %q does not exist in the task directory", sf)
		}
	}

	// Deadline ordering (17).
	if t.Deadline.Soft != nil && t.Deadline.Hard != nil && t.Deadline.Soft.After(*t.Deadline.Hard) {
		add(SevError, f, "deadline", "soft deadline must be <= hard deadline")
	}

	// Penalty bounds (21).
	p := t.Deadline.Penalty
	if p.Percent < 0 {
		add(SevError, f, "deadline.penalty.percent", "must be >= 0")
	}
	if p.MaxPercent < 0 || p.MaxPercent > 100 {
		add(SevError, f, "deadline.penalty.max_percent", "must be between 0 and 100")
	}
	if p.Percent > 0 && p.Per <= 0 {
		add(SevError, f, "deadline.penalty.per", "must be > 0 when percent > 0")
	}

	// Limits (22).
	if t.Limits.MaxAttempts < 0 {
		add(SevError, f, "limits.max_attempts", "must be >= 0 (0 = unlimited)")
	}
	if t.Limits.Cooldown < 0 {
		add(SevError, f, "limits.cooldown", "must be >= 0")
	}

	validateChecks(t, add)
	validateHidden(t, add)
	validateWorkspace(t, add)
	validatePenaltyWarnings(t, add)
}

// repoRoot derives the course repo root from the task's absolute directory
// and its repo-relative task.yaml path.
func (t *ResolvedTask) repoRoot() string {
	rel := filepath.Dir(t.file)
	if t.file == "" || rel == "." {
		return t.Dir
	}
	return strings.TrimSuffix(t.Dir, string(filepath.Separator)+filepath.ToSlash(rel))
}

func validateWorkspace(t *ResolvedTask, add func(Severity, string, string, string, ...any)) {
	f := t.file
	root := t.repoRoot()
	taskDirRel := filepath.Dir(t.file)

	for i, inc := range t.Workspace.Include {
		field := fmt.Sprintf("workspace.include[%d]", i)
		if filepath.IsAbs(inc) {
			add(SevError, f, field, "%q must be a relative path", inc)
			continue
		}
		clean := filepath.Clean(inc)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			add(SevError, f, field, "%q must not escape the course repo", inc)
			continue
		}
		if _, err := os.Stat(filepath.Join(root, clean)); err != nil {
			add(SevError, f, field, "listed path %q does not exist in the course repo", inc)
			continue
		}
		if t.file != "" && taskDirRel != "." {
			if clean == taskDirRel || strings.HasPrefix(taskDirRel, clean+string(filepath.Separator)) {
				add(SevWarning, f, field, "%q is already exported automatically", inc)
			}
		}
	}
}

func validateChecks(t *ResolvedTask, add func(Severity, string, string, string, ...any)) {
	f := t.file
	if len(t.Checks) == 0 {
		add(SevError, f, "checks", "at least one check is required")
		return
	}

	seen := map[string]bool{}
	var scoredCount, positiveWeightCount int
	for i, ch := range t.Checks {
		field := fmt.Sprintf("checks[%d]", i)
		if ch.Name == "" {
			add(SevError, f, field+".name", "check name is required")
		} else if seen[ch.Name] {
			add(SevError, f, field+".name", "duplicate check name %q", ch.Name)
		} else if strings.ContainsAny(ch.Name, "/ \t\r\n") {
			// Still supported end to end - the log download resolves the name
			// against the submission's results - but it is percent-encoded in
			// the URL and rewritten in the log file name, so two such names can
			// end up sharing one file.
			add(SevWarning, f, field+".name", "%q contains a path separator or whitespace, which is rewritten in the log file name", ch.Name)
		}
		seen[ch.Name] = true
		if ch.Run == "" {
			add(SevError, f, field+".run", "check command is required")
		}
		if ch.Required {
			if ch.Weight != 0 {
				add(SevWarning, f, field+".weight", "weight is ignored for required (gate) checks")
			}
			continue
		}
		if ch.Weight < 0 {
			// Weights are normalized over the non-gate checks (SPEC §4.3): a
			// negative weight shrinks the divisor, so a passed check can score
			// above the task score - or below 0 once the sum turns negative.
			add(SevError, f, field+".weight", "must be >= 0, got %d (weights are normalized over the non-gate checks)", ch.Weight)
		}
		scoredCount++
		if ch.Weight > 0 {
			positiveWeightCount++
		}
	}

	// Rule 23: a scorable task needs at least one non-gate check with weight > 0.
	if scoredCount == 0 || positiveWeightCount == 0 {
		add(SevError, f, "checks", "task needs at least one non-gate check with weight > 0")
		return
	}
	// Rule 24: dead weight (only meaningful once the task is otherwise scorable).
	for i, ch := range t.Checks {
		if !ch.Required && ch.Weight == 0 {
			add(SevWarning, f, fmt.Sprintf("checks[%d].weight", i), "non-gate check %q has weight 0 and never contributes to the score", ch.Name)
		}
	}
}

func validateHidden(t *ResolvedTask, add func(Severity, string, string, string, ...any)) {
	if t.Hidden == nil {
		return
	}
	f := t.file
	h := t.Hidden
	if !validHiddenSource[h.Source] {
		add(SevError, f, "hidden_tests.source", "must be one of git|local, got %q", h.Source)
		return
	}
	if h.Source == "git" {
		switch {
		case h.URL == "":
			add(SevError, f, "hidden_tests.url", "url is required when source is git")
		case urlHasCredentials(h.URL):
			// The URL reaches git's argv and the server log, so an embedded
			// token would leak (SPEC §11, §14). The diagnostic must not echo
			// the URL: it is reported back in the teacher's push output.
			add(SevError, f, "hidden_tests.url", "must not embed credentials; hidden-tests credentials come from the environment (ANYGRADE_HIDDEN_GIT_TOKEN)")
		}
	}
	if h.Source == "local" {
		switch {
		case h.Path == "":
			add(SevError, f, "hidden_tests.path", "path is required when source is local")
		case !filepath.IsAbs(h.Path):
			// A relative path is resolved against the working directory of
			// whoever reads it (server vs `anygrade check`), so its existence
			// cannot be checked here either.
			add(SevWarning, f, "hidden_tests.path", "%q should be an absolute path: it is resolved on the machine that runs the checks", h.Path)
		default:
			if st, err := os.Stat(h.Path); err != nil {
				add(SevWarning, f, "hidden_tests.path", "%q does not exist here; it must exist on the grading server", h.Path)
			} else if !st.IsDir() {
				add(SevWarning, f, "hidden_tests.path", "%q is not a directory", h.Path)
			}
		}
	}
}

// urlHasCredentials reports whether a hidden-tests URL carries userinfo that
// would end up in git's argv and the server log. A bare ssh username
// (git@host:org/repo.git, ssh://git@host/...) is the normal way to address a
// remote and stays allowed; a password, or any userinfo on an http(s) URL, is
// a credential.
func urlHasCredentials(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return false
	}
	if _, ok := u.User.Password(); ok {
		return true
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func validatePenaltyWarnings(t *ResolvedTask, add func(Severity, string, string, string, ...any)) {
	f := t.file
	// Rule 26: penalty explicitly set on the task but no soft deadline to trigger it.
	if raw := t.raw; raw != nil {
		rp := raw.Deadline.Penalty
		if rp.Percent != nil && *rp.Percent > 0 && raw.Deadline.Soft == nil {
			add(SevWarning, f, "deadline.penalty", "penalty is set but there is no soft deadline, so it can never apply")
		}
	}
	// Rule 27: soft deadline present but the penalty cap is 0, disabling penalties.
	if t.Deadline.Soft != nil && t.Deadline.Penalty.Percent > 0 && t.Deadline.Penalty.MaxPercent == 0 {
		add(SevWarning, f, "deadline.penalty.max_percent", "max_percent is 0, so the penalty is effectively disabled")
	}
}
