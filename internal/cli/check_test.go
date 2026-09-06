package cli

import (
	"testing"

	"github.com/ekalinin/anygrade/internal/runner"
)

// TestCheckNote: SPEC §11 - "check" keeps nothing staff-only locally, so a
// check with a build phase always gets its build log path mentioned, next to
// the run log path whenever one is also shown. A check without a build phase
// keeps behaving exactly as before.
func TestCheckNote(t *testing.T) {
	const buildPath = "/data/logs/run/build/basic.log"
	tests := []struct {
		name     string
		outcome  runner.Outcome
		hasBuild bool
		wantRes  string
		wantNote string
	}{
		{
			name:     "pass, no build phase",
			outcome:  runner.Outcome{Passed: true},
			hasBuild: false,
			wantRes:  "pass",
			wantNote: "",
		},
		{
			name:     "pass, build phase",
			outcome:  runner.Outcome{Passed: true},
			hasBuild: true,
			wantRes:  "pass",
			wantNote: "build:" + buildPath,
		},
		{
			name:     "fail, no build phase",
			outcome:  runner.Outcome{Passed: false, LogPath: "/data/logs/run/basic.log"},
			hasBuild: false,
			wantRes:  "fail",
			wantNote: "/data/logs/run/basic.log",
		},
		{
			name:     "fail, build phase",
			outcome:  runner.Outcome{Passed: false, LogPath: "/data/logs/run/basic.log"},
			hasBuild: true,
			wantRes:  "fail",
			wantNote: "/data/logs/run/basic.log  build:" + buildPath,
		},
		{
			name:     "timeout, build phase",
			outcome:  runner.Outcome{TimedOut: true, LogPath: "/data/logs/run/basic.log"},
			hasBuild: true,
			wantRes:  "timeout",
			wantNote: "/data/logs/run/basic.log  build:" + buildPath,
		},
		{
			name:     "skip, build phase",
			outcome:  runner.Outcome{Skipped: true},
			hasBuild: true,
			wantRes:  "skip",
			wantNote: "",
		},
		{
			name:     "build failed",
			outcome:  runner.Outcome{BuildFailed: true, BuildLogPath: buildPath},
			hasBuild: true,
			wantRes:  "build fail",
			wantNote: buildPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, note := checkNote(tt.outcome, tt.hasBuild, buildPath)
			if res != tt.wantRes {
				t.Errorf("res = %q, want %q", res, tt.wantRes)
			}
			if note != tt.wantNote {
				t.Errorf("note = %q, want %q", note, tt.wantNote)
			}
		})
	}
}
