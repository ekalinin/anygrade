package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

// TestCheckLogsAreOwnerOnly: check logs are a student's full output, read
// through the UI - no other account on the host has any business with them.
func TestCheckLogsAreOwnerOnly(t *testing.T) {
	job := localJob(t, time.Minute, []config.Check{{Name: "ok", Weight: 1, Run: "echo hi"}})
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(job.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("log dir mode = %v, want 0700", perm)
	}
	file, err := os.Stat(outcomes[0].LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := file.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %v, want 0600", perm)
	}
}

// TestCheckLogCapsFile: check output is untrusted, so the full log on disk is
// bounded. Writes past the cap are accepted (the check must run to completion)
// but only the marker reaches the file, and the excerpt says so.
func TestCheckLogCapsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build.log")
	log, err := openCheckLog(path, "build", nil, 64, 100)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if n, err := log.Write([]byte(strings.Repeat("x", 100))); err != nil || n != 100 {
			t.Fatalf("write must always succeed: n=%d err=%v", n, err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	data := readFile(t, path)
	if body := strings.SplitN(data, "\n", 2)[0]; len(body) != 100 {
		t.Errorf("log body = %d bytes, want the 100 byte cap", len(body))
	}
	if !strings.Contains(data, "log truncated at 100 bytes") {
		t.Errorf("log file carries no truncation marker: %q", data)
	}
	if !strings.Contains(log.Excerpt(), "log truncated at 100 bytes") {
		t.Errorf("excerpt carries no truncation marker: %q", log.Excerpt())
	}
}

// TestCheckLogUnderCap: the usual case must stay byte-exact and unmarked.
func TestCheckLogUnderCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build.log")
	log, err := openCheckLog(path, "build", nil, 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "hello\n" {
		t.Errorf("log = %q", got)
	}
	if got := log.Excerpt(); got != "hello\n" {
		t.Errorf("excerpt = %q", got)
	}
}

// TestCheckLogWriteError: a failing log write (a full disk hits ENOSPC here
// first) must not cost the check its result - the write is reported as
// successful, the failure surfaces in the excerpt instead.
func TestCheckLogWriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build.log")
	log, err := openCheckLog(path, "build", nil, 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Closing the file under the writer is the portable stand-in for ENOSPC:
	// the next write to the descriptor fails.
	if err := log.file.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := log.Write([]byte("output\n")); err != nil || n != 7 {
		t.Fatalf("write must not fail the check: n=%d err=%v", n, err)
	}
	if _, err := log.Write([]byte("more\n")); err != nil {
		t.Fatalf("writes after a failure must stay silent: %v", err)
	}
	if !strings.Contains(log.Excerpt(), "could not be written") {
		t.Errorf("excerpt hides the failure: %q", log.Excerpt())
	}
	// The check output is still available live and in the excerpt.
	if !strings.Contains(log.Excerpt(), "output") {
		t.Errorf("excerpt lost the output: %q", log.Excerpt())
	}
}

// TestLocalRunnerCapsLogFile is the same cap through a real check: the process
// prints far past the limit, still finishes, and the result is unaffected.
func TestLocalRunnerCapsLogFile(t *testing.T) {
	// 200 lines of 11 bytes = 2200 bytes, far past the 512-byte cap below.
	job := localJob(t, time.Minute, []config.Check{
		{Name: "noisy", Weight: 1, Run: "for i in $(seq 1 200); do echo 0123456789; done"},
	})
	job.Spec.LogMax = 512

	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !outcomes[0].Passed {
		t.Fatalf("a noisy check still has to produce its result: %+v", outcomes[0])
	}
	st, err := os.Stat(outcomes[0].LogPath)
	if err != nil {
		t.Fatal(err)
	}
	// The cap plus the one marker line, nowhere near the ~7 KB printed.
	if st.Size() > 512+128 {
		t.Errorf("log file = %d bytes, want at most the 512 byte cap plus a marker", st.Size())
	}
	if !strings.Contains(outcomes[0].LogExcerpt, "log truncated at 512 bytes") {
		t.Errorf("excerpt: %q", outcomes[0].LogExcerpt)
	}
}
