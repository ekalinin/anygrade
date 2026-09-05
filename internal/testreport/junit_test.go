package testreport

import (
	"strings"
	"testing"
	"time"
)

// junitPassing is what a runner emits for a suite that passes, root element
// and all.
const junitPassing = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="2" failures="0" errors="0" time="0.153">
  <testsuite name="example/sum" tests="2" failures="0" errors="0" time="0.153">
    <testcase classname="example/sum" name="TestAdd" time="0.020"/>
    <testcase classname="example/sum" name="TestSub" time="0.001"/>
  </testsuite>
</testsuites>
`

func TestJUnitPassing(t *testing.T) {
	cases := mustParse(t, JUnitXML, junitPassing)
	if len(cases) != 2 {
		t.Fatalf("want 2 cases, got %+v", cases)
	}
	if cases[0].Name != "TestAdd" || cases[0].Status != Pass {
		t.Errorf("first case: %+v", cases[0])
	}
	if cases[0].Duration != 20*time.Millisecond {
		t.Errorf("time attribute not carried over: %v", cases[0].Duration)
	}
	if p, s := Tally(cases); p != 2 || s != 2 {
		t.Errorf("tally = %d/%d, want 2/2", p, s)
	}
}

// <failure> and <error> both mean the case did not pass; <skipped> counts for
// neither side. A bare <testsuite> root is as common as <testsuites>, so both
// have to read.
func TestJUnitFailuresErrorsAndSkips(t *testing.T) {
	const in = `<testsuite name="pkg" tests="4">
  <testcase name="test_ok" time="0.01"/>
  <testcase name="test_asserts" time="0.02">
    <failure message="expected 5, got 4" type="AssertionError">tests/test_sum.py:11: in test_asserts
    assert sum(2, 2) == 5</failure>
  </testcase>
  <testcase name="test_blows_up">
    <error message="ZeroDivisionError: division by zero"/>
  </testcase>
  <testcase name="test_platform">
    <skipped message="windows only"/>
  </testcase>
</testsuite>
`
	cases := mustParse(t, JUnitXML, in)
	if len(cases) != 4 {
		t.Fatalf("want 4 cases, got %+v", cases)
	}
	want := []Status{Pass, Fail, Fail, Skip}
	for i, w := range want {
		if cases[i].Status != w {
			t.Errorf("%s: status %q, want %q", cases[i].Name, cases[i].Status, w)
		}
	}
	if !strings.Contains(cases[1].Message, "expected 5, got 4") ||
		!strings.Contains(cases[1].Message, "assert sum(2, 2) == 5") {
		t.Errorf("both the message attribute and the body belong in the message: %q", cases[1].Message)
	}
	if !strings.Contains(cases[2].Message, "ZeroDivisionError") {
		t.Errorf("error message missing: %q", cases[2].Message)
	}
	if p, s := Tally(cases); p != 1 || s != 3 {
		t.Errorf("tally = %d/%d, want 1/3 (the skip counts for neither side)", p, s)
	}
}

// Suites nested in suites are legal and common (one file per package under a
// project root). The walk has no schema, so nesting costs it nothing.
func TestJUnitNestedSuites(t *testing.T) {
	const in = `<testsuites>
  <testsuite name="outer">
    <testsuite name="inner">
      <testcase name="deep"/>
    </testsuite>
    <testcase name="shallow"><failure/></testcase>
  </testsuite>
</testsuites>`
	cases := mustParse(t, JUnitXML, in)
	if len(cases) != 2 || cases[0].Name != "deep" || cases[1].Name != "shallow" {
		t.Fatalf("nested suites: %+v", cases)
	}
}

func TestJUnitMalformedFallsBack(t *testing.T) {
	for name, in := range map[string]string{
		"unclosed element":   `<testsuite><testcase name="a"></testsuit>`,
		"truncated document": `<testsuites><testsuite name="p"><testcase name=`,
		"plain text":         "3 passed, 1 failed in 0.15s\n",
		"undefined entity":   `<testsuite><testcase name="&nope;"/></testsuite>`,
		"empty":              "",
	} {
		t.Run(name, func(t *testing.T) {
			if cases, err := Parse(JUnitXML, strings.NewReader(in)); err == nil {
				t.Fatalf("want a parse error, got %+v", cases)
			}
		})
	}
}
