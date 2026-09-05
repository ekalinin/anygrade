// Package testreport turns the machine-readable output of a test runner into
// per-test-case results (SPEC §4.3). The course author names the format on the
// check (`parser:`); anygrade never sniffs it, because a check is an arbitrary
// command in an arbitrary language (SPEC §1) and only the author knows what it
// prints.
//
// The package is deliberately dependency-free - three formats, the standard
// library's JSON and XML decoders, and no knowledge of the rest of anygrade -
// so the one thing it has to get right stays visible: everything it reads is
// produced in the same workspace as the student's solution. Every bound below
// is a safety bound rather than a tuning knob, and a report it cannot read is
// never the student's fault (see Parse).
package testreport

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The accepted values of a check's `parser:` key. This list is the whole
// vocabulary: an unknown value is a metadata error, not a guess.
const (
	None       = "none"         // default: the check is scored by its exit code alone
	GoTestJSON = "go-test-json" // `go test -json`
	JUnitXML   = "junit-xml"    // JUnit/xUnit XML, what most runners can emit
	TAP        = "tap"          // Test Anything Protocol
)

// Formats lists the accepted values in the order a metadata error names them.
func Formats() []string { return []string{None, GoTestJSON, JUnitXML, TAP} }

// Valid reports whether s is an accepted `parser:` value; the empty string is
// the unset key and means None.
func Valid(s string) bool { return s == "" || slices.Contains(Formats(), s) }

// Enabled reports whether s asks for any parsing at all.
func Enabled(s string) bool { return s != "" && s != None }

// Status is the verdict of one test case. The values are exactly the ones the
// check_cases status column accepts, so storage needs no translation table.
type Status string

const (
	Pass Status = "passed"
	Fail Status = "failed"
	Skip Status = "skipped"
)

// Case is one test case of a check's report.
type Case struct {
	Name     string
	Status   Status
	Duration time.Duration
	Message  string // failure detail or skip reason; never kept for a passed case
}

// The bounds below all guard the same thing: the report is written by a test
// run that executes the student's own code, so its size is the student's to
// choose. A test that prints a megabyte per case must not put a megabyte per
// case in the database (SPEC §14), and none of these can cost a check its
// result - going over any of them falls back to the exit code.
const (
	// MaxInput is the largest report Parse reads. A bigger one is refused
	// rather than truncated: half a report has the wrong denominator, while
	// the exit code it falls back to is never wrong.
	MaxInput = 4 << 20
	// MaxCases bounds the rows one check may store. Refused rather than cut
	// off for the same reason as MaxInput - and the flood that reaches it can
	// only come from output nobody planned, since a course with 1000 real
	// cases in one check would have noticed.
	MaxCases = 1000
	// MaxName and MaxMessage bound one case; MaxMessages bounds every message
	// of one report together, the way runner.log_excerpt bounds the log.
	MaxName     = 200
	MaxMessage  = 512
	MaxMessages = 64 << 10
)

var (
	// ErrTooLarge, ErrTooManyCases and ErrNoCases are the three ways a report
	// can be unusable without being malformed. All three read the same to the
	// caller: the check keeps its exit code.
	ErrTooLarge     = fmt.Errorf("report is larger than %d bytes", MaxInput)
	ErrTooManyCases = fmt.Errorf("report holds more than %d test cases", MaxCases)
	ErrNoCases      = errors.New("report holds no test case")
)

// Parse reads a report in the named format.
//
// Any error it returns is the parser's problem and never the student's: the
// caller's contract is to fall back to the check's exit code and say so, which
// is exactly the behaviour of a course that configures no parser at all. That
// is why an empty report is an error here too - a check with no readable cases
// has no proportion to be scored by.
func Parse(format string, r io.Reader) ([]Case, error) {
	if !Enabled(format) {
		return nil, fmt.Errorf("no parser configured")
	}
	// One byte over the bound is enough to tell "fits" from "too big" without
	// ever holding the rest of an oversized report.
	data, err := io.ReadAll(io.LimitReader(r, MaxInput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxInput {
		return nil, ErrTooLarge
	}

	var cases []Case
	switch format {
	case GoTestJSON:
		cases, err = parseGoTest(data)
	case JUnitXML:
		cases, err = parseJUnit(data)
	case TAP:
		cases, err = parseTAP(data)
	default:
		return nil, fmt.Errorf("unknown parser %q", format)
	}
	if err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, ErrNoCases
	}
	return bound(cases), nil
}

// Tally counts what a check's score is a fraction of. A skipped case is
// neither earned nor lost, so it counts for neither side; a report of nothing
// but skips therefore yields scored == 0 and sends the check back to the
// all-or-nothing verdict of its exit code (SPEC §4.3).
func Tally(cases []Case) (passed, scored int) {
	for _, c := range cases {
		switch c.Status {
		case Pass:
			passed++
			scored++
		case Fail:
			scored++
		}
	}
	return passed, scored
}

// bound applies the storage bounds to a parsed report. It runs once, in Parse,
// so no parser can forget it.
func bound(cases []Case) []Case {
	budget := MaxMessages
	for i := range cases {
		c := &cases[i]
		c.Name = cleanName(c.Name)
		if c.Status == Pass {
			// A passed case has nothing to explain, and it is the one an
			// ordinary run produces by the hundred.
			c.Message = ""
			continue
		}
		c.Message = cleanMessage(c.Message, min(MaxMessage, budget))
		budget -= len(c.Message)
	}
	return cases
}

// cleanName makes a case name safe to store and to render: valid UTF-8, one
// line, bounded. html/template escapes the markup; the control characters go
// here, because a carriage return in a name would still be free to rewrite a
// log line or an SSE frame on its way through.
func cleanName(s string) string {
	return truncate(strings.Map(dropControl, strings.ToValidUTF8(strings.TrimSpace(s), "")), MaxName)
}

// cleanMessage keeps the line breaks - a failure message is several lines and
// the page renders it as such - and drops every other control character.
func cleanMessage(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		return dropControl(r)
	}, strings.ReplaceAll(s, "\r\n", "\n"))
	return truncate(strings.TrimSpace(s), max)
}

// dropControl removes C0/C1 control characters, turning a tab into a space so
// indented output does not collapse into one word.
func dropControl(r rune) rune {
	switch {
	case r == '\t':
		return ' '
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return -1
	}
	return r
}

// truncateMark ends a value the bounds cut short, so a reader can tell a
// truncated name from a name that really ends there.
const truncateMark = "…"

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max - len(truncateMark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut-- // never cut a rune in half: the result has to stay valid UTF-8
	}
	if cut <= 0 {
		return ""
	}
	return s[:cut] + truncateMark
}

// seconds converts an elapsed time in seconds, as every one of the three
// formats reports it, into a Duration. Anything that is not a positive number
// (absent, negative, NaN) is no duration at all rather than a nonsense one.
func seconds(v float64) time.Duration {
	if !(v > 0) {
		return 0
	}
	return time.Duration(v * float64(time.Second))
}

// parseSeconds is seconds for the formats that carry the value as text.
func parseSeconds(s string) time.Duration {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return seconds(v)
}
