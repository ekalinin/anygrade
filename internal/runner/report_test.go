package runner

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/testreport"
)

// TestParserReadsRunPhaseOutput: the default source is the run phase's own
// output, which is where `go test -json` and TAP put the report.
func TestParserReadsRunPhaseOutput(t *testing.T) {
	job := localJob(t, time.Minute, []config.Check{{
		Name: "unit", Weight: 100, Parser: testreport.TAP,
		// Two of three pass and the command fails, which is the shape the
		// proportional score exists for.
		Run: "printf '1..3\\nok 1 - a\\nnot ok 2 - b\\nok 3 - c\\n'; exit 1",
	}})
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	o := outcomes[0]
	if o.Passed || o.ParseFailed {
		t.Fatalf("check: %+v", o)
	}
	if len(o.Cases) != 3 {
		t.Fatalf("cases: %+v", o.Cases)
	}
	if p, s := testreport.Tally(o.Cases); p != 2 || s != 3 {
		t.Errorf("tally = %d/%d, want 2/3", p, s)
	}
}

// TestParserReadsReportFile: `parser_file:` reads a file out of the workspace
// instead, which is how a format that is a file by convention (JUnit XML)
// reaches the parser at all.
func TestParserReadsReportFile(t *testing.T) {
	job := localJob(t, time.Minute, []config.Check{{
		Name: "suite", Weight: 100, Parser: testreport.JUnitXML, ParserFile: "report.xml",
		Run: `printf '%s' '<testsuite name="s"><testcase name="one"/>` +
			`<testcase name="two"><failure message="nope"/></testcase></testsuite>' > report.xml`,
	}})
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	o := outcomes[0]
	if o.ParseFailed || len(o.Cases) != 2 {
		t.Fatalf("check: %+v cases: %+v", o, o.Cases)
	}
	if o.Cases[0].Name != "one" || o.Cases[1].Status != testreport.Fail {
		t.Errorf("cases: %+v", o.Cases)
	}
	// The file is not the log: the check's own output stays the excerpt.
	if strings.Contains(o.LogExcerpt, "testcase") {
		t.Errorf("the report file leaked into the log excerpt: %q", o.LogExcerpt)
	}
}

// TestParserFailureKeepsTheExitCode is the constraint the whole feature is
// bounded by: a parser must never turn a passing run into a failing one. Every
// way of not producing a report ends with the check's own exit code and a flag
// saying why there is no list.
func TestParserFailureKeepsTheExitCode(t *testing.T) {
	tests := []struct {
		name  string
		check config.Check
		pass  bool
	}{
		{"unparseable output", config.Check{
			Name: "a", Weight: 1, Parser: testreport.TAP,
			Run: "echo 'all good, 3 tests'; exit 0",
		}, true},
		{"wrong format", config.Check{
			Name: "b", Weight: 1, Parser: testreport.JUnitXML,
			Run: "printf '1..1\\nok 1 - a\\n'; exit 0",
		}, true},
		{"missing report file", config.Check{
			Name: "c", Weight: 1, Parser: testreport.JUnitXML, ParserFile: "report.xml",
			Run: "echo no file written; exit 0",
		}, true},
		{"a failing check stays failed", config.Check{
			Name: "d", Weight: 1, Parser: testreport.TAP,
			Run: "echo nonsense; exit 3",
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcomes, err := (&LocalRunner{}).Run(t.Context(), localJob(t, time.Minute, []config.Check{tc.check}))
			if err != nil {
				t.Fatal(err)
			}
			o := outcomes[0]
			if o.Passed != tc.pass {
				t.Errorf("the exit code decides: Passed = %v, want %v", o.Passed, tc.pass)
			}
			if !o.ParseFailed {
				t.Errorf("a check whose report could not be read has to say so: %+v", o)
			}
			if len(o.Cases) != 0 {
				t.Errorf("no cases may be invented: %+v", o.Cases)
			}
		})
	}
}

// A `parser_file:` that walks out of the workspace is refused rather than
// followed. Metadata validation rejects it too, but the value reaches a file
// open, so it is checked where it is used.
func TestParserFileCannotEscapeTheWorkspace(t *testing.T) {
	job := localJob(t, time.Minute, []config.Check{{
		Name: "escape", Weight: 1, Parser: testreport.JUnitXML,
		ParserFile: "../../report.xml", Run: "true",
	}})
	outside := filepath.Join(filepath.Dir(filepath.Dir(job.WorkspaceDir)), "report.xml")
	writeFiles(t, filepath.Dir(outside), map[string]string{
		"report.xml": `<testsuite><testcase name="stolen"/></testsuite>`,
	})

	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if o := outcomes[0]; !o.ParseFailed || len(o.Cases) != 0 {
		t.Errorf("a path outside the workspace must not be read: %+v", o.Cases)
	}
}

// The phases a parser never touches: a check that failed while being built has
// only teacher-only output (SPEC §14), a timed-out one has half a report, and
// a skipped one has none. None of them may end up with cases - or, for the
// last two, with a note claiming a parser tried.
func TestParserSkipsPhasesItMayNotRead(t *testing.T) {
	job := localJob(t, 300*time.Millisecond, []config.Check{
		{Name: "built", Weight: 1, Parser: testreport.TAP,
			Build: "printf '1..1\\nok 1 - built\\n' >&2; exit 1", Run: "printf '1..1\\nok 1 - ran\\n'"},
		{Name: "hangs", Weight: 1, Parser: testreport.TAP,
			Run: "printf '1..2\\nok 1 - first\\n'; sleep 30 & wait"},
		{Name: "gate", Required: true, Run: "false"},
		{Name: "skipped", Weight: 1, Parser: testreport.TAP, Run: "printf '1..1\\nok 1 - never\\n'"},
	})
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outcomes {
		if len(o.Cases) != 0 {
			t.Errorf("%s: %+v", o.Name, o.Cases)
		}
	}
	if !outcomes[0].BuildFailed || outcomes[0].ParseFailed {
		t.Errorf("a build failure is not a parse failure: %+v", outcomes[0])
	}
	if !outcomes[1].TimedOut || outcomes[1].ParseFailed {
		t.Errorf("a timeout is not a parse failure either: %+v", outcomes[1])
	}
	if !outcomes[3].Skipped || outcomes[3].ParseFailed {
		t.Errorf("a check that never ran has nothing to parse: %+v", outcomes[3])
	}
}

// A check without a `parser:` is untouched, which is the compatibility
// guarantee: every existing course keeps its rows exactly as they were.
func TestNoParserLeavesTheOutcomeAlone(t *testing.T) {
	job := localJob(t, time.Minute, []config.Check{
		{Name: "plain", Weight: 1, Run: "printf '1..1\\nok 1 - a\\n'"},
		{Name: "explicit-none", Weight: 1, Parser: testreport.None, Run: "printf '1..1\\nok 1 - a\\n'"},
	})
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outcomes {
		if !o.Passed || o.ParseFailed || len(o.Cases) != 0 {
			t.Errorf("%s: %+v", o.Name, o)
		}
	}
}
