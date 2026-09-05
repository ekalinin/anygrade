package testreport

import (
	"strings"
	"testing"
	"time"
)

// goPassing is `go test -json` over a package where everything passes, with
// the package-level events the stream really carries.
const goPassing = `{"Time":"2026-09-05T10:00:00.1Z","Action":"start","Package":"example/sum"}
{"Time":"2026-09-05T10:00:00.2Z","Action":"run","Package":"example/sum","Test":"TestAdd"}
{"Time":"2026-09-05T10:00:00.2Z","Action":"output","Package":"example/sum","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}
{"Time":"2026-09-05T10:00:00.2Z","Action":"output","Package":"example/sum","Test":"TestAdd","Output":"--- PASS: TestAdd (0.00s)\n"}
{"Time":"2026-09-05T10:00:00.2Z","Action":"pass","Package":"example/sum","Test":"TestAdd","Elapsed":0.02}
{"Time":"2026-09-05T10:00:00.3Z","Action":"run","Package":"example/sum","Test":"TestSub"}
{"Time":"2026-09-05T10:00:00.3Z","Action":"pass","Package":"example/sum","Test":"TestSub","Elapsed":0}
{"Time":"2026-09-05T10:00:00.4Z","Action":"output","Package":"example/sum","Output":"PASS\n"}
{"Time":"2026-09-05T10:00:00.4Z","Action":"pass","Package":"example/sum","Elapsed":0.152}
`

func TestGoTestPassing(t *testing.T) {
	cases := mustParse(t, GoTestJSON, goPassing)
	if len(cases) != 2 {
		t.Fatalf("want 2 cases (package events are not tests), got %d: %+v", len(cases), cases)
	}
	if cases[0].Name != "TestAdd" || cases[0].Status != Pass {
		t.Errorf("first case: %+v", cases[0])
	}
	if cases[0].Duration != 20*time.Millisecond {
		t.Errorf("elapsed not carried over: %v", cases[0].Duration)
	}
	if cases[0].Message != "" {
		t.Errorf("a passed case keeps no message: %q", cases[0].Message)
	}
	if p, s := Tally(cases); p != 2 || s != 2 {
		t.Errorf("tally = %d/%d, want 2/2", p, s)
	}
}

// A failing run is the one that has to be right: the verdict per case, the
// failure text as the message, and a tally the score can be a fraction of.
func TestGoTestFailures(t *testing.T) {
	const out = `{"Action":"run","Package":"p","Test":"TestAdd"}
{"Action":"pass","Package":"p","Test":"TestAdd","Elapsed":0}
{"Action":"run","Package":"p","Test":"TestSub"}
{"Action":"output","Package":"p","Test":"TestSub","Output":"=== RUN   TestSub\n"}
{"Action":"output","Package":"p","Test":"TestSub","Output":"    sum_test.go:19: Sub(3, 1) = 1, want 2\n"}
{"Action":"output","Package":"p","Test":"TestSub","Output":"--- FAIL: TestSub (0.00s)\n"}
{"Action":"fail","Package":"p","Test":"TestSub","Elapsed":0.01}
{"Action":"fail","Package":"p","Elapsed":0.02}
`
	cases := mustParse(t, GoTestJSON, out)
	if len(cases) != 2 {
		t.Fatalf("want 2 cases, got %+v", cases)
	}
	if cases[1].Status != Fail {
		t.Errorf("TestSub should have failed: %+v", cases[1])
	}
	if !strings.Contains(cases[1].Message, "want 2") {
		t.Errorf("failure text missing from the message: %q", cases[1].Message)
	}
	if strings.Contains(cases[1].Message, "=== RUN") || strings.Contains(cases[1].Message, "--- FAIL") {
		t.Errorf("test2json framing should not fill the message budget: %q", cases[1].Message)
	}
	if p, s := Tally(cases); p != 1 || s != 2 {
		t.Errorf("tally = %d/%d, want 1/2", p, s)
	}
}

// A skipped case is neither earned nor lost: it is shown, and it counts for
// neither side of the fraction (SPEC §4.3).
func TestGoTestSkips(t *testing.T) {
	const out = `{"Action":"run","Package":"p","Test":"TestNet"}
{"Action":"output","Package":"p","Test":"TestNet","Output":"    net_test.go:9: needs network\n"}
{"Action":"skip","Package":"p","Test":"TestNet","Elapsed":0}
{"Action":"run","Package":"p","Test":"TestLocal"}
{"Action":"pass","Package":"p","Test":"TestLocal","Elapsed":0}
`
	cases := mustParse(t, GoTestJSON, out)
	if cases[0].Status != Skip {
		t.Fatalf("TestNet should be skipped: %+v", cases[0])
	}
	if !strings.Contains(cases[0].Message, "needs network") {
		t.Errorf("skip reason missing: %q", cases[0].Message)
	}
	if p, s := Tally(cases); p != 1 || s != 1 {
		t.Errorf("tally = %d/%d, want 1/1: a skip counts for neither side", p, s)
	}
}

// Subtests: the parent is reported alongside its children, so counting both
// would make a table test worth one case more than it has and would charge a
// failing subtest twice.
func TestGoTestSubtestsCountOnce(t *testing.T) {
	const out = `{"Action":"run","Package":"p","Test":"TestTable"}
{"Action":"run","Package":"p","Test":"TestTable/positive"}
{"Action":"pass","Package":"p","Test":"TestTable/positive","Elapsed":0}
{"Action":"run","Package":"p","Test":"TestTable/negative"}
{"Action":"fail","Package":"p","Test":"TestTable/negative","Elapsed":0}
{"Action":"run","Package":"p","Test":"TestTable/negative/zero"}
{"Action":"fail","Package":"p","Test":"TestTable/negative/zero","Elapsed":0}
{"Action":"fail","Package":"p","Test":"TestTable","Elapsed":0.01}
`
	cases := mustParse(t, GoTestJSON, out)
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.Name
	}
	want := []string{"TestTable/positive", "TestTable/negative/zero"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("leaf tests only: got %v, want %v", names, want)
	}
	if p, s := Tally(cases); p != 1 || s != 2 {
		t.Errorf("tally = %d/%d, want 1/2", p, s)
	}
}

// The stream is the check's stdout AND stderr, so noise around the report is
// the normal case, not the broken one.
func TestGoTestIgnoresNonJSONNoise(t *testing.T) {
	const out = `go: downloading example.com/dep v1.2.3
{"Action":"run","Package":"p","Test":"TestAdd"}
warning: something on stderr
{"Action":"pass","Package":"p","Test":"TestAdd","Elapsed":0}
{"Action":"pass","Package":"p",
`
	cases := mustParse(t, GoTestJSON, out)
	if len(cases) != 1 || cases[0].Status != Pass {
		t.Fatalf("noise should be skipped, the report read: %+v", cases)
	}
}

// A test the stream never finished (the binary died under it) has no verdict,
// so it is not a case: reporting it as failed would invent a result.
func TestGoTestUnfinishedTestIsNotACase(t *testing.T) {
	const out = `{"Action":"run","Package":"p","Test":"TestOK"}
{"Action":"pass","Package":"p","Test":"TestOK","Elapsed":0}
{"Action":"run","Package":"p","Test":"TestPanics"}
{"Action":"output","Package":"p","Test":"TestPanics","Output":"panic: boom\n"}
`
	cases := mustParse(t, GoTestJSON, out)
	if len(cases) != 1 || cases[0].Name != "TestOK" {
		t.Fatalf("only finished tests are cases: %+v", cases)
	}
}

// The fallback contract, per format: unreadable output is the parser's fault,
// so it must produce an error the caller can turn into "scored by exit code" -
// never a case list that would score the student.
func TestGoTestMalformedFallsBack(t *testing.T) {
	for name, in := range map[string]string{
		"plain go test output": "=== RUN   TestAdd\n--- PASS: TestAdd (0.00s)\nPASS\nok  \texample/sum\t0.152s\n",
		"truncated json":       `{"Action":"pass","Package":"p","Test":"TestAdd"`,
		"not a report at all":  "Segmentation fault\n",
		"empty":                "",
	} {
		t.Run(name, func(t *testing.T) {
			if cases, err := Parse(GoTestJSON, strings.NewReader(in)); err == nil {
				t.Fatalf("want a parse error, got %+v", cases)
			}
		})
	}
}
