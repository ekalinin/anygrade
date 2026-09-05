package cli

import (
	"archive/zip"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/ident"
	"github.com/ekalinin/anygrade/internal/store"
)

// baseCodeDir holds the task template inside the corpus: the authoritative
// solution_files every student started from. A similarity checker has to be
// told to subtract them (JPlag `--bc`, MOSS `-b`), or the shared skeleton is
// the strongest match in the whole run. The leading underscore is what keeps
// the directory from ever colliding with a student's: ident.ValidLogin
// requires a login to start with a letter or a digit.
const baseCodeDir = "_template"

// attemptSep joins a login and a submission id when every attempt is
// exported. It is deliberately a character ident.ValidLogin forbids, so
// "alice@12" can never be some other student's directory.
const attemptSep = "@"

// The corpus is student code taken out of repos that are owner-only, so it
// does not become the widest thing in the data dir's neighbourhood.
const (
	corpusDirMode  = 0o700
	corpusFileMode = 0o600
)

// exportSubmissions implements `anygrade export submissions` (SPEC §11): the
// per-task corpus a plagiarism checker consumes. anygrade compares nothing
// itself - the threshold and the tool stay the teacher's.
func exportSubmissions(args []string) int {
	fs := flag.NewFlagSet("export submissions", flag.ContinueOnError)
	taskID := fs.String("task", "", "task id to export (required)")
	format := fs.String("format", "dir", "output format: dir|zip")
	out := fs.String("out", "-", "output directory (dir) or archive file (zip); \"-\" writes the zip to stdout")
	allAttempts := fs.Bool("all-attempts", false,
		"export every recorded submission, not only the one the scoring policy counts")
	repoFlag := fs.String("repo", "", "course repo root (default: git toplevel, else \".\")")
	dataFlag := fs.String("data-dir", "", "anygrade data directory (default: <repo>/.anygrade)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *taskID == "" {
		fmt.Fprintln(os.Stderr, "export: --task is required")
		return 2
	}
	if *format != "dir" && *format != "zip" {
		fmt.Fprintf(os.Stderr, "export: --format must be dir or zip, got %q\n", *format)
		return 2
	}
	if *format == "dir" && *out == "-" {
		fmt.Fprintln(os.Stderr, "export: --format dir needs --out DIR; only a zip can go to stdout")
		return 2
	}

	_, dataDir, err := exportDirs(*repoFlag, *dataFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		return 2
	}

	ctx := context.Background()
	rm := gitserver.RepoManager{DataDir: dataDir}
	courseDir := rm.CourseDir()
	if _, err := os.Stat(courseDir); err != nil {
		// Unlike the score matrix, this export reads the bare repos, and they
		// exist only next to the mirror. A working copy cannot stand in: it
		// holds the teacher's tree, not any student's.
		fmt.Fprintf(os.Stderr, "anygrade export: no course mirror in %s; "+
			"the submissions export reads the server's repos\n", dataDir)
		return 1
	}
	course, code := loadCourseMirror(ctx, courseDir)
	if code != 0 {
		return code
	}
	task, relDir, ok := course.Task(*taskID)
	if !ok {
		fmt.Fprintf(os.Stderr, "anygrade export: unknown task %q; the course has: %s\n",
			*taskID, strings.Join(taskIDs(course.Resolved.Tasks), ", "))
		return 2
	}

	users, subs, err := readCorpusRows(ctx, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return 1
	}
	entries := selectCorpus(users, subs, *taskID, course.Resolved.Course.ScoringPolicy, *allAttempts)

	w, err := newCorpusWriter(*format, *out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return 1
	}
	job := corpusJob{
		Entries:    entries,
		Task:       task,
		RelDir:     relDir,
		Course:     gitserver.GitSource{Dir: courseDir, Commit: course.Head},
		StudentDir: rm.StudentDir,
	}
	written, failed, err := writeCorpus(ctx, w, job, exportWarn)
	closeErr := w.Close()
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return 1
	case closeErr != nil:
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", closeErr)
		return 1
	}
	fmt.Fprintf(os.Stderr, "anygrade export: task %s: %d submission(s) exported, %d skipped\n",
		*taskID, written, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// exportWarn is where every per-submission problem goes. It is stderr even
// when the archive itself is stdout, so a piped zip stays a zip.
func exportWarn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "anygrade export: "+format+"\n", a...)
}

func taskIDs(tasks []config.ResolvedTask) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return ids
}

// readCorpusRows takes everything the export needs out of the database and
// closes it again, so git never runs with a query in flight (AGENTS.md).
func readCorpusRows(ctx context.Context, dataDir string) ([]store.User, []store.Submission, error) {
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	users, err := db.ListUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	subs, err := db.ListAllSubmissions(ctx)
	if err != nil {
		return nil, nil, err
	}
	return users, subs, nil
}

// corpusEntry is one exported tree: whose code it is, which submission it came
// from, and the corpus-root-relative directory it lands in.
type corpusEntry struct {
	Login string
	Sub   store.Submission
	Dir   string
}

// selectCorpus picks the submissions to export for one task, in login order.
//
// Without allAttempts this is exactly the submission the gradebook counts:
// gradebook.Winner is the single implementation of the best|latest rule
// (SPEC §9), so the corpus cannot disagree with the grade a teacher would have
// to defend. With allAttempts every recorded submission of the pair is
// exported instead - it is what catches "copied, then rewrote", at the price
// of a corpus that grows with the number of pushes, which is why it is not the
// default.
//
// Each attempt gets its own top-level directory rather than a subdirectory
// under the student's: a checker treats every entry of the corpus root as one
// submission, so nesting the attempts would fuse them into a single blob.
func selectCorpus(users []store.User, subs []store.Submission,
	taskID, policy string, allAttempts bool) []corpusEntry {

	byUser := map[int64][]store.Submission{}
	for _, s := range subs {
		if s.TaskID == taskID {
			byUser[s.UserID] = append(byUser[s.UserID], s)
		}
	}

	students := make([]store.User, 0, len(users))
	for _, u := range users {
		if u.Role == "student" {
			students = append(students, u)
		}
	}
	slices.SortFunc(students, func(a, b store.User) int {
		return strings.Compare(a.Login, b.Login)
	})

	var out []corpusEntry
	for _, u := range students {
		history := byUser[u.ID]
		if allAttempts {
			for _, s := range history {
				out = append(out, corpusEntry{Login: u.Login, Sub: s,
					Dir: u.Login + attemptSep + strconv.FormatInt(s.ID, 10)})
			}
			continue
		}
		if win := gradebook.Winner(history, policy); win != nil {
			out = append(out, corpusEntry{Login: u.Login, Sub: *win, Dir: u.Login})
		}
	}
	return out
}

// corpusJob is everything the export needs once the flags and the database
// have been read. No store handle reaches it by design.
type corpusJob struct {
	Entries    []corpusEntry
	Task       config.ResolvedTask
	RelDir     string              // task dir relative to the repo root (slash-separated)
	Course     gitserver.GitSource // course mirror at the graded head
	StudentDir func(login string) string
}

// writeCorpus renders the job into w, returning how many student trees were
// written and how many were skipped for a reason the operator has to know
// about.
//
// A per-submission problem is reported through warn and the export continues:
// a corpus missing one student is still the corpus the teacher asked for, and
// stopping at the first unreadable repo would hide every later one. Only a
// failure of the output itself (a full disk, an unwritable archive) aborts.
func writeCorpus(ctx context.Context, w corpusWriter, j corpusJob,
	warn func(string, ...any)) (written, failed int, err error) {

	// The base code first: it is the one tree that is always present, so a
	// teacher pointing JPlag at it never has to guess whether it made it in.
	for _, sf := range j.Task.SolutionFiles {
		rel, err := corpusPath(baseCodeDir, sf)
		if err != nil {
			return 0, 0, err
		}
		data, ok, err := j.Course.File(ctx, path.Join(j.RelDir, sf))
		if err != nil {
			return 0, 0, fmt.Errorf("read the task template %q: %w", sf, err)
		}
		if !ok {
			warn("the task template has no %q, so the base code will not cover it", sf)
			continue
		}
		if err := w.writeFile(rel, data); err != nil {
			return 0, 0, err
		}
	}

	for _, e := range j.Entries {
		// Defence in depth: ValidLogin already rules out both a component
		// that could escape a path and the leading underscore of the base
		// code directory, so a login failing here was not written by
		// anygrade - and it is about to become a directory name and a zip
		// entry either way.
		if !ident.ValidLogin(e.Login) || e.Dir == baseCodeDir {
			warn("submission %d: %q is not a usable directory name; skipped", e.Sub.ID, e.Login)
			failed++
			continue
		}
		dir := j.StudentDir(e.Login)
		ref := fmt.Sprintf("refs/anygrade/submissions/%d", e.Sub.ID)
		// The pin, not submissions.commit_sha: the ref is what makes a graded
		// commit survive a force push (SPEC §6 step 7), so if it is gone the
		// object may be gone too, and reading the sha would be a guess.
		commit, err := gitserver.Git(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err != nil {
			warn("submission %d (%s): %s is gone; the commit is no longer pinned and cannot be exported",
				e.Sub.ID, e.Login, ref)
			failed++
			continue
		}

		src := gitserver.GitSource{Dir: dir, Commit: commit}
		files := 0
		for _, sf := range j.Task.SolutionFiles {
			rel, err := corpusPath(e.Dir, sf)
			if err != nil {
				return written, failed, err
			}
			data, ok, err := src.File(ctx, path.Join(j.RelDir, sf))
			switch {
			case err != nil:
				warn("submission %d (%s): %s: %v", e.Sub.ID, e.Login, sf, err)
				continue
			case !ok:
				// The student never added the file, or deleted it: grading used
				// the template in its place (SPEC §6.1). Writing the template
				// here instead would make every such student look identical to
				// every other, which is the one thing this export exists to
				// avoid.
				warn("submission %d (%s): %s is absent from the submitted commit", e.Sub.ID, e.Login, sf)
				continue
			}
			if err := w.writeFile(rel, data); err != nil {
				return written, failed, err
			}
			files++
		}
		if files == 0 {
			// No directory was created: the writers only make one when a file
			// goes in it, and an empty submission is noise a checker has to be
			// told to ignore.
			warn("submission %d (%s): no solution file present, nothing exported", e.Sub.ID, e.Login)
			continue
		}
		written++
	}
	return written, failed, nil
}

// corpusPath is the single place a name out of the database or the course
// metadata becomes a path. The same string is a directory component on one
// side and a zip entry name on the other, and an entry that escapes the root
// is a real bug class in both, so the check happens once, on the joined path,
// for every file written.
func corpusPath(dir, file string) (string, error) {
	rel := path.Join(dir, filepath.ToSlash(file))
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("export path %q escapes the corpus root", path.Join(dir, file))
	}
	return rel, nil
}

// corpusWriter is the only thing the two --format values differ in.
type corpusWriter interface {
	writeFile(rel string, data []byte) error
	Close() error
}

func newCorpusWriter(format, out string) (corpusWriter, error) {
	if format == "dir" {
		return newDirCorpus(out)
	}
	if out == "-" {
		return &zipCorpus{zw: zip.NewWriter(os.Stdout)}, nil
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, corpusFileMode)
	if err != nil {
		return nil, err
	}
	return &zipCorpus{zw: zip.NewWriter(f), file: f}, nil
}

// dirCorpus writes the corpus as a plain tree. Every write goes through an
// os.Root, so a symlink an earlier export (or anything else) left in the
// output directory cannot redirect student code out of it.
type dirCorpus struct{ root *os.Root }

func newDirCorpus(dir string) (*dirCorpus, error) {
	if err := os.MkdirAll(dir, corpusDirMode); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &dirCorpus{root: root}, nil
}

func (d *dirCorpus) writeFile(rel string, data []byte) error {
	name := filepath.FromSlash(rel)
	if parent := filepath.Dir(name); parent != "." {
		if err := d.root.MkdirAll(parent, corpusDirMode); err != nil {
			return err
		}
	}
	f, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, corpusFileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (d *dirCorpus) Close() error { return d.root.Close() }

// zipCorpus writes the same tree into one archive. Entry names are the corpus
// paths verbatim, so unzipping into a directory reproduces the --format dir
// output exactly.
type zipCorpus struct {
	zw   *zip.Writer
	file io.Closer // nil when the archive goes to stdout
}

func (z *zipCorpus) writeFile(rel string, data []byte) error {
	w, err := z.zw.Create(rel)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func (z *zipCorpus) Close() error {
	err := z.zw.Close()
	if z.file != nil {
		if cerr := z.file.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
