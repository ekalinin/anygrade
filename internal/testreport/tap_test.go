package testreport

import (
	"strings"
	"testing"
)

// tapPassing is a complete TAP 13 stream: version line, plan, results.
const tapPassing = `TAP version 13
1..3
ok 1 - adds two numbers
ok 2 - adds zero
ok 3 - adds negatives
`

func TestTAPPassing(t *testing.T) {
	cases := mustParse(t, TAP, tapPassing)
	if len(cases) != 3 {
		t.Fatalf("want 3 cases (the version line and the plan are not results), got %+v", cases)
	}
	if cases[0].Name != "adds two numbers" || cases[0].Status != Pass {
		t.Errorf("first case: %+v", cases[0])
	}
	if p, s := Tally(cases); p != 3 || s != 3 {
		t.Errorf("tally = %d/%d, want 3/3", p, s)
	}
}

func TestTAPFailuresAndDiagnostics(t *testing.T) {
	const in = `TAP version 13
1..2
ok 1 - adds two numbers
not ok 2 - subtracts
  ---
  message: 'expected 2, got 1'
  severity: fail
  ...
# a bare comment at column 0 is not part of the result
`
	cases := mustParse(t, TAP, in)
	if len(cases) != 2 {
		t.Fatalf("want 2 cases, got %+v", cases)
	}
	if cases[1].Status != Fail || cases[1].Name != "subtracts" {
		t.Errorf("second case: %+v", cases[1])
	}
	if !strings.Contains(cases[1].Message, "expected 2, got 1") {
		t.Errorf("the YAML block belongs to the result above it: %q", cases[1].Message)
	}
	if strings.Contains(cases[1].Message, "bare comment") {
		t.Errorf("an unindented comment is not diagnostics: %q", cases[1].Message)
	}
	if p, s := Tally(cases); p != 1 || s != 2 {
		t.Errorf("tally = %d/%d, want 1/2", p, s)
	}
}

// SKIP did not run; TODO is known to be broken and TAP says it must not count
// as a failure. Both are "neither side" for the score, whichever verdict the
// line carries.
func TestTAPDirectives(t *testing.T) {
	const in = `1..3
ok 1 - runs everywhere
ok 2 - windows only # SKIP not applicable here
not ok 3 - rounding # TODO known broken
`
	cases := mustParse(t, TAP, in)
	if cases[1].Status != Skip || cases[2].Status != Skip {
		t.Fatalf("directives: %+v", cases)
	}
	if !strings.Contains(cases[1].Message, "not applicable here") {
		t.Errorf("the directive is the skip reason: %q", cases[1].Message)
	}
	if p, s := Tally(cases); p != 1 || s != 1 {
		t.Errorf("tally = %d/%d, want 1/1", p, s)
	}
}

// A result line has no obligation to describe itself, and a word that merely
// starts with "ok" is not a verdict.
func TestTAPLineShapes(t *testing.T) {
	const in = `ok
oklahoma is not a result
not ok
ok 7
`
	cases := mustParse(t, TAP, in)
	if len(cases) != 3 {
		t.Fatalf("want 3 results, got %+v", cases)
	}
	if cases[0].Name != "test 1" || cases[1].Name != "test 2" || cases[2].Name != "test 3" {
		t.Errorf("a result with no description is named by its position: %+v", cases)
	}
	if cases[0].Status != Pass || cases[1].Status != Fail || cases[2].Status != Pass {
		t.Errorf("verdicts: %+v", cases)
	}
}

func TestTAPMalformedFallsBack(t *testing.T) {
	for name, in := range map[string]string{
		"prose":         "All tests passed!\n2 tests, 0 failures\n",
		"plan only":     "TAP version 13\n1..5\n",
		"bail out only": "Bail out! the harness died\n",
		"empty":         "",
	} {
		t.Run(name, func(t *testing.T) {
			if cases, err := Parse(TAP, strings.NewReader(in)); err == nil {
				t.Fatalf("want a parse error, got %+v", cases)
			}
		})
	}
}
