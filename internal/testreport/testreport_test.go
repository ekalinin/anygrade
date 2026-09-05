package testreport

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustParse(t *testing.T, format, in string) []Case {
	t.Helper()
	cases, err := Parse(format, strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse(%s): %v", format, err)
	}
	return cases
}

func TestValidFormats(t *testing.T) {
	for _, ok := range []string{"", None, GoTestJSON, JUnitXML, TAP} {
		if !Valid(ok) {
			t.Errorf("Valid(%q) = false", ok)
		}
	}
	for _, bad := range []string{"go-test", "junit", "xunit", "NONE", "go test -json"} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
	// Enabled is what turns the key into work: an unset key and "none" are the
	// default behaviour, which is no parsing at all.
	if Enabled("") || Enabled(None) || !Enabled(TAP) {
		t.Error("Enabled disagrees with the default")
	}
	if _, err := Parse(None, strings.NewReader("ok 1 - x\n")); err == nil {
		t.Error("Parse(none) must not parse anything")
	}
	if _, err := Parse("junit", strings.NewReader("ok 1 - x\n")); err == nil {
		t.Error("Parse of an unknown format must fail")
	}
}

func TestTally(t *testing.T) {
	got := []Case{{Status: Pass}, {Status: Fail}, {Status: Skip}, {Status: Pass}}
	if p, s := Tally(got); p != 2 || s != 3 {
		t.Errorf("tally = %d/%d, want 2/3", p, s)
	}
	// Nothing but skips leaves no proportion to score by, which is what sends
	// the check back to its exit code.
	if p, s := Tally([]Case{{Status: Skip}}); p != 0 || s != 0 {
		t.Errorf("tally = %d/%d, want 0/0", p, s)
	}
	if p, s := Tally(nil); p != 0 || s != 0 {
		t.Errorf("tally = %d/%d, want 0/0", p, s)
	}
}

// The report is written by a run of the student's own code, so its size is the
// student's to choose. Each bound refuses the report rather than storing it,
// which costs only the proportional scoring - the exit code still decides.
func TestInputBound(t *testing.T) {
	big := "ok 1 - x\n" + strings.Repeat("# padding\n", MaxInput/10)
	if len(big) <= MaxInput {
		t.Fatalf("fixture is not over the bound: %d", len(big))
	}
	if _, err := Parse(TAP, strings.NewReader(big)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestCaseCountBound(t *testing.T) {
	var b strings.Builder
	for i := range MaxCases + 1 {
		fmt.Fprintf(&b, "ok %d - case\n", i+1)
	}
	if _, err := Parse(TAP, strings.NewReader(b.String())); !errors.Is(err, ErrTooManyCases) {
		t.Fatalf("want ErrTooManyCases, got %v", err)
	}
	// One under the bound still parses: the limit is a ceiling, not a target.
	var ok strings.Builder
	for i := range MaxCases {
		fmt.Fprintf(&ok, "ok %d - case\n", i+1)
	}
	if cases := mustParse(t, TAP, ok.String()); len(cases) != MaxCases {
		t.Fatalf("want %d cases, got %d", MaxCases, len(cases))
	}
}

// A test that prints a megabyte per case must not put a megabyte per case in
// the database: the name, each message, and every message together are all
// bounded, and a passed case stores no message at all.
func TestStoredSizeIsBounded(t *testing.T) {
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, "not ok %d - %s\n", i+1, strings.Repeat("n", 4000))
		for range 5 {
			fmt.Fprintf(&b, "  %s\n", strings.Repeat("m", 1000))
		}
	}
	fmt.Fprintf(&b, "ok 201 - passed\n  %s\n", strings.Repeat("m", 1000))

	cases := mustParse(t, TAP, b.String())
	total := 0
	for _, c := range cases {
		if len(c.Name) > MaxName {
			t.Fatalf("name of %d bytes exceeds the bound", len(c.Name))
		}
		if len(c.Message) > MaxMessage {
			t.Fatalf("message of %d bytes exceeds the bound", len(c.Message))
		}
		total += len(c.Message)
	}
	if total > MaxMessages {
		t.Errorf("messages total %d bytes, bound is %d", total, MaxMessages)
	}
	if last := cases[len(cases)-1]; last.Status != Pass || last.Message != "" {
		t.Errorf("a passed case keeps no message: %+v", last)
	}
}

// Names and messages are student-controlled text that ends up in a template
// and in a log line, so they leave here as valid UTF-8 without control
// characters. Markup is not rewritten - escaping it is the template's job and
// mangling it here would only hide what the test was called.
func TestNamesAreSanitized(t *testing.T) {
	in := "ok 1 - <b>bold</b>\x1b[31m red \x00 nul \xff invalid\n"
	cases := mustParse(t, TAP, in)
	name := cases[0].Name
	if !utf8.ValidString(name) {
		t.Errorf("name is not valid UTF-8: %q", name)
	}
	if strings.ContainsAny(name, "\x00\x1b\r\n") {
		t.Errorf("control characters survived: %q", name)
	}
	if !strings.Contains(name, "<b>bold</b>") {
		t.Errorf("markup must be preserved for the template to escape: %q", name)
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	got := truncate(strings.Repeat("ы", MaxName), MaxName)
	if !utf8.ValidString(got) {
		t.Errorf("truncated value is not valid UTF-8: %q", got)
	}
	if len(got) > MaxName {
		t.Errorf("truncate returned %d bytes, bound is %d", len(got), MaxName)
	}
	if !strings.HasSuffix(got, truncateMark) {
		t.Errorf("a truncated value has to say so: %q", got)
	}
}
