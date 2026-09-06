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

// A parent that fails on its own after every subtest passed - a post-loop
// assertion here - is a real result, not the merely redundant parent line
// dropParents otherwise erases (#122). Captured with `go test -json` from:
//
//	func TestParent(t *testing.T) {
//		t.Run("a", func(t *testing.T) {})
//		t.Run("b", func(t *testing.T) {})
//		t.Errorf("post-loop invariant broken")
//	}
const goFailedParentPassingChildren = `{"Action":"start","Package":"example.com/parenttest"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestParent"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestParent","Output":"=== RUN   TestParent\n"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestParent/a"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestParent/a","Output":"=== RUN   TestParent/a\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestParent/a","Output":"--- PASS: TestParent/a (0.00s)\n"}
{"Action":"pass","Package":"example.com/parenttest","Test":"TestParent/a","Elapsed":0}
{"Action":"run","Package":"example.com/parenttest","Test":"TestParent/b"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestParent/b","Output":"=== RUN   TestParent/b\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestParent/b","Output":"--- PASS: TestParent/b (0.00s)\n"}
{"Action":"pass","Package":"example.com/parenttest","Test":"TestParent/b","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Test":"TestParent","Output":"    parent_test.go:8: post-loop invariant broken\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestParent","Output":"--- FAIL: TestParent (0.00s)\n"}
{"Action":"fail","Package":"example.com/parenttest","Test":"TestParent","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Output":"FAIL\n"}
{"Action":"output","Package":"example.com/parenttest","Output":"FAIL\texample.com/parenttest\t0.187s\n"}
{"Action":"fail","Package":"example.com/parenttest","Elapsed":0.188}
`

func TestGoTestFailedParentKeptWithPassingChildren(t *testing.T) {
	cases := mustParse(t, GoTestJSON, goFailedParentPassingChildren)
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.Name
	}
	want := []string{"TestParent", "TestParent/a", "TestParent/b"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("parent kept alongside its children: got %v, want %v", names, want)
	}
	parent := cases[0]
	if parent.Status != Fail {
		t.Fatalf("TestParent failed on its own and must be reported as failed: %+v", parent)
	}
	if !strings.Contains(parent.Message, "post-loop invariant broken") {
		t.Errorf("failure text missing from the parent's message: %q", parent.Message)
	}
	if p, s := Tally(cases); p != 2 || s != 3 {
		t.Errorf("tally = %d/%d, want 2/3", p, s)
	}
}

// A parent whose subtests all pass is still just the redundant summary line:
// dropping it is unchanged behaviour. Captured from:
//
//	func TestAllPass(t *testing.T) {
//		t.Run("a", func(t *testing.T) {})
//		t.Run("b", func(t *testing.T) {})
//	}
const goAllPassingParentDropped = `{"Action":"start","Package":"example.com/parenttest"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestAllPass"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllPass","Output":"=== RUN   TestAllPass\n"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestAllPass/a"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllPass/a","Output":"=== RUN   TestAllPass/a\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllPass/a","Output":"--- PASS: TestAllPass/a (0.00s)\n"}
{"Action":"pass","Package":"example.com/parenttest","Test":"TestAllPass/a","Elapsed":0}
{"Action":"run","Package":"example.com/parenttest","Test":"TestAllPass/b"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllPass/b","Output":"=== RUN   TestAllPass/b\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllPass/b","Output":"--- PASS: TestAllPass/b (0.00s)\n"}
{"Action":"pass","Package":"example.com/parenttest","Test":"TestAllPass/b","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllPass","Output":"--- PASS: TestAllPass (0.00s)\n"}
{"Action":"pass","Package":"example.com/parenttest","Test":"TestAllPass","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Output":"PASS\n"}
{"Action":"output","Package":"example.com/parenttest","Output":"ok  \texample.com/parenttest\t0.167s\n"}
{"Action":"pass","Package":"example.com/parenttest","Elapsed":0.168}
`

func TestGoTestSubtestsAllPassingDropsParent(t *testing.T) {
	cases := mustParse(t, GoTestJSON, goAllPassingParentDropped)
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.Name
	}
	want := []string{"TestAllPass/a", "TestAllPass/b"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("a passing parent stays dropped: got %v, want %v", names, want)
	}
	if p, s := Tally(cases); p != 2 || s != 2 {
		t.Errorf("tally = %d/%d, want 2/2", p, s)
	}
}

// "Every descendant passed" in the keep rule includes a skipped one: a
// skipped subtest is neither a pass nor the fail that would explain the
// parent's own. Captured from:
//
//	func TestMixed(t *testing.T) {
//		t.Run("a", func(t *testing.T) {})
//		t.Run("b", func(t *testing.T) {
//			t.Skip("not applicable")
//		})
//		t.Errorf("post-loop invariant broken")
//	}
const goFailedParentWithSkippedChild = `{"Action":"start","Package":"example.com/parenttest"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestMixed"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestMixed","Output":"=== RUN   TestMixed\n"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestMixed/a"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestMixed/a","Output":"=== RUN   TestMixed/a\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestMixed/a","Output":"--- PASS: TestMixed/a (0.00s)\n"}
{"Action":"pass","Package":"example.com/parenttest","Test":"TestMixed/a","Elapsed":0}
{"Action":"run","Package":"example.com/parenttest","Test":"TestMixed/b"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestMixed/b","Output":"=== RUN   TestMixed/b\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestMixed/b","Output":"    parent_test.go:8: not applicable\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestMixed/b","Output":"--- SKIP: TestMixed/b (0.00s)\n"}
{"Action":"skip","Package":"example.com/parenttest","Test":"TestMixed/b","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Test":"TestMixed","Output":"    parent_test.go:10: post-loop invariant broken\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestMixed","Output":"--- FAIL: TestMixed (0.00s)\n"}
{"Action":"fail","Package":"example.com/parenttest","Test":"TestMixed","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Output":"FAIL\n"}
{"Action":"output","Package":"example.com/parenttest","Output":"FAIL\texample.com/parenttest\t0.178s\n"}
{"Action":"fail","Package":"example.com/parenttest","Elapsed":0.178}
`

func TestGoTestFailedParentKeptWithSkippedChild(t *testing.T) {
	cases := mustParse(t, GoTestJSON, goFailedParentWithSkippedChild)
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.Name
	}
	want := []string{"TestMixed", "TestMixed/a", "TestMixed/b"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("parent kept alongside its passing and skipped children: got %v, want %v", names, want)
	}
	if cases[0].Status != Fail {
		t.Fatalf("TestMixed failed on its own and must be reported as failed: %+v", cases[0])
	}
	if cases[2].Status != Skip {
		t.Fatalf("TestMixed/b was skipped, not failed: %+v", cases[2])
	}
	if p, s := Tally(cases); p != 1 || s != 2 {
		t.Errorf("tally = %d/%d, want 1/2: the skip counts on neither side", p, s)
	}
}

// The all-skip extreme of the same rule: every descendant of the failing
// parent is skipped, none passed, and the parent still stays. Captured from:
//
//	func TestAllSkip(t *testing.T) {
//		t.Run("a", func(t *testing.T) {
//			t.Skip("skip a")
//		})
//		t.Run("b", func(t *testing.T) {
//			t.Skip("skip b")
//		})
//		t.Errorf("post-loop invariant broken")
//	}
const goFailedParentWithAllSkippedChildren = `{"Action":"start","Package":"example.com/parenttest"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestAllSkip"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip","Output":"=== RUN   TestAllSkip\n"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestAllSkip/a"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip/a","Output":"=== RUN   TestAllSkip/a\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip/a","Output":"    parent_test.go:7: skip a\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip/a","Output":"--- SKIP: TestAllSkip/a (0.00s)\n"}
{"Action":"skip","Package":"example.com/parenttest","Test":"TestAllSkip/a","Elapsed":0}
{"Action":"run","Package":"example.com/parenttest","Test":"TestAllSkip/b"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip/b","Output":"=== RUN   TestAllSkip/b\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip/b","Output":"    parent_test.go:10: skip b\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip/b","Output":"--- SKIP: TestAllSkip/b (0.00s)\n"}
{"Action":"skip","Package":"example.com/parenttest","Test":"TestAllSkip/b","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip","Output":"    parent_test.go:12: post-loop invariant broken\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestAllSkip","Output":"--- FAIL: TestAllSkip (0.00s)\n"}
{"Action":"fail","Package":"example.com/parenttest","Test":"TestAllSkip","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Output":"FAIL\n"}
{"Action":"output","Package":"example.com/parenttest","Output":"FAIL\texample.com/parenttest\t0.230s\n"}
{"Action":"fail","Package":"example.com/parenttest","Elapsed":0.23}
`

func TestGoTestFailedParentKeptWithAllSkippedChildren(t *testing.T) {
	cases := mustParse(t, GoTestJSON, goFailedParentWithAllSkippedChildren)
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.Name
	}
	want := []string{"TestAllSkip", "TestAllSkip/a", "TestAllSkip/b"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("parent kept alongside its two skipped children: got %v, want %v", names, want)
	}
	if cases[0].Status != Fail {
		t.Fatalf("TestAllSkip failed on its own and must be reported as failed: %+v", cases[0])
	}
	if p, s := Tally(cases); p != 0 || s != 1 {
		t.Errorf("tally = %d/%d, want 0/1: only the parent scores, both children are skips", p, s)
	}
}

// Three levels deep: TestOuter/mid fails on its own after its only child
// passed, so it stays; TestOuter itself fails only because TestOuter/mid
// failed, so it is still dropped. Captured from:
//
//	func TestOuter(t *testing.T) {
//		t.Run("mid", func(t *testing.T) {
//			t.Run("leaf", func(t *testing.T) {})
//			t.Errorf("mid-level invariant broken")
//		})
//	}
const goNestedParentFailsOnItsOwn = `{"Action":"start","Package":"example.com/parenttest"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestOuter"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestOuter","Output":"=== RUN   TestOuter\n"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestOuter/mid"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestOuter/mid","Output":"=== RUN   TestOuter/mid\n"}
{"Action":"run","Package":"example.com/parenttest","Test":"TestOuter/mid/leaf"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestOuter/mid/leaf","Output":"=== RUN   TestOuter/mid/leaf\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestOuter/mid/leaf","Output":"--- PASS: TestOuter/mid/leaf (0.00s)\n"}
{"Action":"pass","Package":"example.com/parenttest","Test":"TestOuter/mid/leaf","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Test":"TestOuter/mid","Output":"    parent_test.go:8: mid-level invariant broken\n"}
{"Action":"output","Package":"example.com/parenttest","Test":"TestOuter/mid","Output":"--- FAIL: TestOuter/mid (0.00s)\n"}
{"Action":"fail","Package":"example.com/parenttest","Test":"TestOuter/mid","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Test":"TestOuter","Output":"--- FAIL: TestOuter (0.00s)\n"}
{"Action":"fail","Package":"example.com/parenttest","Test":"TestOuter","Elapsed":0}
{"Action":"output","Package":"example.com/parenttest","Output":"FAIL\n"}
{"Action":"output","Package":"example.com/parenttest","Output":"FAIL\texample.com/parenttest\t0.194s\n"}
{"Action":"fail","Package":"example.com/parenttest","Elapsed":0.194}
`

func TestGoTestNestedParentFailsOnItsOwn(t *testing.T) {
	cases := mustParse(t, GoTestJSON, goNestedParentFailsOnItsOwn)
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.Name
	}
	want := []string{"TestOuter/mid", "TestOuter/mid/leaf"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("the middle node stays, the outer summary line does not: got %v, want %v", names, want)
	}
	if cases[0].Status != Fail {
		t.Fatalf("TestOuter/mid failed on its own: %+v", cases[0])
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
