package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

// --- selection -------------------------------------------------------------

func doneSub(id, user int64, task string, score float64) store.Submission {
	return store.Submission{ID: id, UserID: user, TaskID: task,
		Status: store.StatusDone, FinalScore: &score}
}

// TestSelectCorpusFollowsScoringPolicy: the exported tree has to be the tree
// the grade came from, under either policy. The assertion is against
// gradebook.DisplayScore rather than a hand-written expectation, so the two
// cannot drift apart without the test noticing.
func TestSelectCorpusFollowsScoringPolicy(t *testing.T) {
	users := []store.User{
		{ID: 1, Login: "alice", Role: "student"},
		{ID: 2, Login: "bob", Role: "student"},
		{ID: 3, Login: "teach", Role: "teacher"},
	}
	// received_at ascending, the order ListAllSubmissions returns.
	subs := []store.Submission{
		doneSub(1, 1, "t1", 40),
		doneSub(2, 1, "t1", 90),
		doneSub(3, 1, "t1", 70),
		doneSub(4, 1, "t2", 10), // another task: never in this corpus
		doneSub(5, 2, "t1", 55),
		{ID: 6, UserID: 3, TaskID: "t1", Status: store.StatusDone}, // teacher
	}

	for _, tc := range []struct {
		policy string
		want   map[string]int64 // login -> submission id
	}{
		{policy: "best", want: map[string]int64{"alice": 2, "bob": 5}},
		{policy: "latest", want: map[string]int64{"alice": 3, "bob": 5}},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			got := selectCorpus(users, subs, "t1", tc.policy, false)
			if len(got) != len(tc.want) {
				t.Fatalf("selected %d entries, want %d: %+v", len(got), len(tc.want), got)
			}
			if got[0].Login != "alice" || got[1].Login != "bob" {
				t.Fatalf("entries are not in login order: %+v", got)
			}
			for _, e := range got {
				if e.Sub.ID != tc.want[e.Login] {
					t.Errorf("%s: exported submission %d, want %d", e.Login, e.Sub.ID, tc.want[e.Login])
				}
				if e.Dir != e.Login {
					t.Errorf("%s: directory %q, want the bare login", e.Login, e.Dir)
				}
				// The whole point: the same submission the gradebook counts.
				var history []store.Submission
				for _, s := range subs {
					if s.UserID == e.Sub.UserID && s.TaskID == "t1" {
						history = append(history, s)
					}
				}
				want := gradebook.DisplayScore(history, tc.policy)
				if want == nil || e.Sub.FinalScore == nil || *want != *e.Sub.FinalScore {
					t.Errorf("%s: exported score %v, gradebook shows %v", e.Login, e.Sub.FinalScore, want)
				}
			}
		})
	}
}

// TestSelectCorpusAllAttempts: the opt-in widens the corpus to every recorded
// submission and gives each its own top-level directory, keyed by the id a
// teacher can look up in the UI.
func TestSelectCorpusAllAttempts(t *testing.T) {
	users := []store.User{{ID: 1, Login: "alice", Role: "student"}}
	subs := []store.Submission{
		doneSub(1, 1, "t1", 40),
		doneSub(2, 1, "t1", 90),
		// Not done, so no score: still code the student wrote, and exactly the
		// "copied, then rewrote" evidence --all-attempts exists for.
		{ID: 3, UserID: 1, TaskID: "t1", Status: store.StatusRejectedDeadline},
	}

	one := selectCorpus(users, subs, "t1", "best", false)
	if len(one) != 1 || one[0].Sub.ID != 2 {
		t.Fatalf("default selection = %+v, want only submission 2", one)
	}
	all := selectCorpus(users, subs, "t1", "best", true)
	if len(all) != 3 {
		t.Fatalf("--all-attempts selected %d entries, want 3: %+v", len(all), all)
	}
	for i, want := range []string{"alice@1", "alice@2", "alice@3"} {
		if all[i].Dir != want {
			t.Errorf("entry %d directory = %q, want %q", i, all[i].Dir, want)
		}
	}
}

// TestCorpusPathRefusesEscape guards the one bug class shared by the two
// output formats: a name that leaves the corpus root.
func TestCorpusPathRefusesEscape(t *testing.T) {
	if _, err := corpusPath("alice", "../../etc/passwd"); err == nil {
		t.Error("a solution path climbing out of the root was accepted")
	}
	if _, err := corpusPath("..", "main.go"); err == nil {
		t.Error("a directory climbing out of the root was accepted")
	}
	got, err := corpusPath("alice", "src/main.go")
	if err != nil || got != "alice/src/main.go" {
		t.Errorf("corpusPath = %q, %v; want alice/src/main.go", got, err)
	}
}

// --- writing ---------------------------------------------------------------

// corpusFixture is a provisioned data dir: a course mirror with one task, two
// student repos, and a pinned commit per submission.
type corpusFixture struct {
	dataDir string
	repos   *gitserver.RepoManager
	course  *intake.Course
	task    config.ResolvedTask
	relDir  string
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newCorpusFixture(t *testing.T) *corpusFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	courseSrc := t.TempDir()
	gitT(t, courseSrc, "init", "-b", "main")
	writeTree(t, courseSrc, map[string]string{
		"course.yaml": "name: Corpus course\nregistration:\n  mode: invite\n" +
			"defaults:\n  runner:\n    type: local\n    timeout: 1m\n",
		"tasks/t1/task.yaml": "name: T1\nscore: 100\nsolution_files:\n  - main.go\n" +
			"checks:\n  - name: run\n    weight: 1\n    run: \"true\"\n",
		"tasks/t1/main.go":      "package main // template\n",
		"tasks/t1/open_test.go": "package main // open test\n",
		"tasks/t1/README.md":    "task readme\n",
	})
	gitT(t, courseSrc, "add", "-A")
	gitT(t, courseSrc, "commit", "-m", "course init")

	dataDir := t.TempDir()
	repos := &gitserver.RepoManager{DataDir: dataDir, HookBin: "/usr/bin/true"}
	if err := repos.EnsureCourse(t.Context(), courseSrc); err != nil {
		t.Fatal(err)
	}
	course, diags, err := intake.LoadCourse(t.Context(), repos.CourseDir())
	if err != nil || course == nil {
		t.Fatalf("LoadCourse: %v %v", err, diags)
	}
	task, relDir, ok := course.Task("t1")
	if !ok {
		t.Fatal("task t1 is missing from the fixture course")
	}
	return &corpusFixture{dataDir: dataDir, repos: repos, course: course, task: task, relDir: relDir}
}

// submit provisions login's repo if needed, pushes files (removing remove),
// and pins the result at refs/anygrade/submissions/<subID>, exactly as intake
// does (SPEC §6).
func (f *corpusFixture) submit(t *testing.T, login string, subID int64,
	files map[string]string, remove ...string) {

	t.Helper()
	bare, err := f.repos.EnsureStudent(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), login)
	gitT(t, ".", "clone", "--quiet", bare, work)
	writeTree(t, work, files)
	for _, p := range remove {
		if err := os.Remove(filepath.Join(work, filepath.FromSlash(p))); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-m", fmt.Sprintf("submission %d", subID))
	gitT(t, work, "push", "--quiet", "origin", "main")
	head := gitT(t, work, "rev-parse", "HEAD")
	gitT(t, bare, "update-ref", fmt.Sprintf("refs/anygrade/submissions/%d", subID), head)
}

func (f *corpusFixture) job(entries []corpusEntry) corpusJob {
	return corpusJob{
		Entries:    entries,
		Task:       f.task,
		RelDir:     f.relDir,
		Course:     gitserver.GitSource{Dir: f.repos.CourseDir(), Commit: f.course.Head},
		StudentDir: f.repos.StudentDir,
	}
}

// collectWarnings returns a warn func and the slice it appends to.
func collectWarnings() (func(string, ...any), *[]string) {
	var got []string
	return func(format string, a ...any) {
		got = append(got, fmt.Sprintf(format, a...))
	}, &got
}

func readCorpusFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestWriteCorpusSolutionFilesOnly: the corpus carries the student's
// solution_files and the task template, and nothing else. Everything the
// students share - the open test, the readme, task.yaml - would be identical
// by construction and would drown the signal the export exists for.
func TestWriteCorpusSolutionFilesOnly(t *testing.T) {
	f := newCorpusFixture(t)
	f.submit(t, "alice", 1, map[string]string{
		"tasks/t1/main.go":      "package main // alice\n",
		"tasks/t1/open_test.go": "package main // alice edited the open test\n",
		"tasks/t1/scratch.txt":  "notes\n",
	})
	f.submit(t, "bob", 2, map[string]string{"tasks/t1/main.go": "package main // bob\n"})

	out := filepath.Join(t.TempDir(), "corpus")
	w, err := newDirCorpus(out)
	if err != nil {
		t.Fatal(err)
	}
	warn, warnings := collectWarnings()
	written, failed, err := writeCorpus(t.Context(), w, f.job([]corpusEntry{
		{Login: "alice", Sub: store.Submission{ID: 1}, Dir: "alice"},
		{Login: "bob", Sub: store.Submission{ID: 2}, Dir: "bob"},
	}), warn)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if written != 2 || failed != 0 {
		t.Fatalf("written=%d failed=%d, want 2 and 0 (warnings: %v)", written, failed, *warnings)
	}

	if got := readCorpusFile(t, out, "alice/main.go"); got != "package main // alice\n" {
		t.Errorf("alice/main.go = %q", got)
	}
	if got := readCorpusFile(t, out, "bob/main.go"); got != "package main // bob\n" {
		t.Errorf("bob/main.go = %q", got)
	}
	for _, unwanted := range []string{
		"alice/open_test.go", "alice/scratch.txt", "alice/task.yaml", "alice/README.md",
	} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(unwanted))); err == nil {
			t.Errorf("%s was exported; only solution_files may be", unwanted)
		}
	}
	// The paths are relative to the task dir, not to the repo root: a checker
	// compares files, not directory layouts.
	if _, err := os.Stat(filepath.Join(out, "alice", "tasks")); err == nil {
		t.Error("the repo-relative task path leaked into the corpus")
	}
}

// TestWriteCorpusEmitsBaseCode: the template lands in a directory a login
// cannot be, and an entry claiming that name is refused rather than allowed to
// overwrite it.
func TestWriteCorpusEmitsBaseCode(t *testing.T) {
	f := newCorpusFixture(t)
	f.submit(t, "alice", 1, map[string]string{"tasks/t1/main.go": "package main // alice\n"})

	out := filepath.Join(t.TempDir(), "corpus")
	w, err := newDirCorpus(out)
	if err != nil {
		t.Fatal(err)
	}
	warn, warnings := collectWarnings()
	_, failed, err := writeCorpus(t.Context(), w, f.job([]corpusEntry{
		{Login: "alice", Sub: store.Submission{ID: 1}, Dir: "alice"},
		// Neither of these can come out of ident.ValidLogin; both are what a
		// hand-edited row would look like.
		{Login: baseCodeDir, Sub: store.Submission{ID: 2}, Dir: baseCodeDir},
		{Login: "../escape", Sub: store.Submission{ID: 3}, Dir: "../escape"},
	}), warn)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if got := readCorpusFile(t, out, baseCodeDir+"/main.go"); got != "package main // template\n" {
		t.Errorf("%s/main.go = %q, want the authoritative template", baseCodeDir, got)
	}
	if failed != 2 {
		t.Errorf("failed = %d, want the two unusable logins counted: %v", failed, *warnings)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(out), "escape")); err == nil {
		t.Error("an entry escaped the corpus root")
	}
}

// TestWriteCorpusZipStaysInsideRoot: every archive entry is a relative path
// inside the archive, which is what keeps `unzip` from writing anywhere the
// teacher did not ask for.
func TestWriteCorpusZipStaysInsideRoot(t *testing.T) {
	f := newCorpusFixture(t)
	f.submit(t, "alice", 1, map[string]string{"tasks/t1/main.go": "package main // alice\n"})

	var buf bytes.Buffer
	w := &zipCorpus{zw: zip.NewWriter(&buf)}
	warn, _ := collectWarnings()
	if _, _, err := writeCorpus(t.Context(), w, f.job([]corpusEntry{
		{Login: "alice", Sub: store.Submission{ID: 1}, Dir: "alice"},
	}), warn); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range zr.File {
		if !filepath.IsLocal(filepath.FromSlash(e.Name)) {
			t.Errorf("zip entry %q escapes the archive root", e.Name)
		}
		names[e.Name] = true
	}
	for _, want := range []string{"alice/main.go", baseCodeDir + "/main.go"} {
		if !names[want] {
			t.Errorf("zip is missing %q; has %v", want, names)
		}
	}
}

// TestWriteCorpusMissingPin: a submission whose pinned ref is gone is reported
// and counted, and the rest of the corpus is still written. A missing solution
// file is a warning only - the student simply never added it.
func TestWriteCorpusMissingPin(t *testing.T) {
	f := newCorpusFixture(t)
	f.submit(t, "alice", 1, map[string]string{"tasks/t1/main.go": "package main // alice\n"})
	// bob pushed something that is not the solution file, and his pin was
	// gc'd afterwards; carol has a pin at a commit without main.go.
	f.submit(t, "bob", 2, map[string]string{"tasks/t1/main.go": "package main // bob\n"})
	gitT(t, f.repos.StudentDir("bob"), "update-ref", "-d", "refs/anygrade/submissions/2")
	f.submit(t, "carol", 3, map[string]string{"tasks/t1/README.md": "carol was here\n"},
		"tasks/t1/main.go")

	out := filepath.Join(t.TempDir(), "corpus")
	w, err := newDirCorpus(out)
	if err != nil {
		t.Fatal(err)
	}
	warn, warnings := collectWarnings()
	written, failed, err := writeCorpus(t.Context(), w, f.job([]corpusEntry{
		{Login: "alice", Sub: store.Submission{ID: 1}, Dir: "alice"},
		{Login: "bob", Sub: store.Submission{ID: 2}, Dir: "bob"},
		{Login: "carol", Sub: store.Submission{ID: 3}, Dir: "carol"},
	}), warn)
	if err != nil {
		t.Fatalf("a gone pin must not abort the export: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if written != 1 {
		t.Errorf("written = %d, want alice only (warnings: %v)", written, *warnings)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want the gone pin counted (warnings: %v)", failed, *warnings)
	}
	if _, err := os.Stat(filepath.Join(out, "bob")); err == nil {
		t.Error("a directory was created for a submission with no readable commit")
	}
	joined := strings.Join(*warnings, "\n")
	if !strings.Contains(joined, "no longer pinned") {
		t.Errorf("no clear message about the gone pin: %v", *warnings)
	}
	if !strings.Contains(joined, "absent from the submitted commit") {
		t.Errorf("no clear message about the missing solution file: %v", *warnings)
	}
	if got := readCorpusFile(t, out, "alice/main.go"); got != "package main // alice\n" {
		t.Errorf("alice/main.go = %q", got)
	}
}

// TestExportSubmissionsUnknownTask: an unknown id is a usage error naming the
// ids that do exist, not a panic and not an empty corpus.
func TestExportSubmissionsUnknownTask(t *testing.T) {
	f := newCorpusFixture(t)
	out := filepath.Join(t.TempDir(), "corpus")
	code := exportSubmissions([]string{"--task", "nope", "--data-dir", f.dataDir, "--out", out})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("an output directory was created for a task that does not exist")
	}
}

// TestExportSubmissionsFlagErrors covers the two flag combinations that cannot
// produce anything, so they are refused before any repo is opened.
func TestExportSubmissionsFlagErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no task", []string{"--data-dir", t.TempDir()}},
		{"unknown format", []string{"--task", "t1", "--format", "tar", "--data-dir", t.TempDir()}},
		{"a directory cannot go to stdout", []string{"--task", "t1", "--format", "dir", "--data-dir", t.TempDir()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := exportSubmissions(tc.args); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
		})
	}
}

// TestExportSubmissionsEndToEnd drives the subcommand the way a teacher does,
// through the store, and checks that the tree it lands on is the documented
// layout.
func TestExportSubmissionsEndToEnd(t *testing.T) {
	f := newCorpusFixture(t)
	f.submit(t, "alice", 1, map[string]string{"tasks/t1/main.go": "package main // alice\n"})

	ctx := context.Background()
	db, err := store.Open(ctx, f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "alice", "Alice", "student")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := db.Enqueue(ctx, store.NewSubmission{
		UserID: user.ID, TaskID: "t1", CommitSHA: "deadbeef", Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimNext(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSubmission(ctx, sub.ID, store.SubmissionResult{
		Status: store.StatusDone, Raw: 80, Final: 80,
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// The fixture pins submission 1; Enqueue hands out id 1 on a fresh DB.
	if sub.ID != 1 {
		t.Fatalf("fixture assumption broken: submission id = %d", sub.ID)
	}

	out := filepath.Join(t.TempDir(), "corpus")
	if code := exportSubmissions([]string{
		"--task", "t1", "--data-dir", f.dataDir, "--out", out,
	}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := readCorpusFile(t, out, "alice/main.go"); got != "package main // alice\n" {
		t.Errorf("alice/main.go = %q", got)
	}
	if got := readCorpusFile(t, out, baseCodeDir+"/main.go"); got != "package main // template\n" {
		t.Errorf("%s/main.go = %q", baseCodeDir, got)
	}
}
