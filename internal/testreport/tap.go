package testreport

import (
	"bytes"
	"fmt"
	"strings"
)

// parseTAP reads the Test Anything Protocol (TAP 13/14).
//
// One result per line, at the start of the line. Everything else - the version
// line, the `1..n` plan, comments, a `Bail out!` - is skipped: a plan that
// disagrees with the results is a harness detail, while the results are what
// the score is a proportion of. Indented lines belong to the result above them
// (the YAML diagnostic block of TAP 13, the subtests of TAP 14), so they are
// read as its message rather than as results of their own.
func parseTAP(data []byte) ([]Case, error) {
	var cases []Case
	for line := range bytes.Lines(data) {
		s := strings.TrimRight(string(line), "\r\n")
		if s == "" {
			continue
		}
		if s[0] == ' ' || s[0] == '\t' {
			if n := len(cases); n > 0 {
				addDetail(&cases[n-1], strings.TrimSpace(s))
			}
			continue
		}
		ok, rest, isResult := cutResult(s)
		if !isResult {
			continue
		}
		if len(cases) >= MaxCases {
			return nil, ErrTooManyCases
		}
		cases = append(cases, tapCase(ok, rest, len(cases)+1))
	}
	return cases, nil
}

// cutResult recognizes a result line and returns everything after its verdict.
// The verdict has to be a whole word: "oklahoma" is a comment somebody forgot
// to mark, not a passing test.
func cutResult(s string) (ok bool, rest string, isResult bool) {
	for _, v := range []struct {
		prefix string
		ok     bool
	}{{"not ok", false}, {"ok", true}} {
		r, found := strings.CutPrefix(s, v.prefix)
		if !found {
			continue
		}
		if r == "" || r[0] == ' ' || r[0] == '\t' {
			return v.ok, r, true
		}
		return false, "", false
	}
	return false, "", false
}

// tapCase reads the description and the directive of one result line. seq is
// the line's position among the results, used when it carries no description.
func tapCase(ok bool, rest string, seq int) Case {
	rest = strings.TrimSpace(rest)
	// The test number is optional and can only repeat the position we are
	// already counting, so it is dropped rather than trusted.
	rest = strings.TrimSpace(strings.TrimLeft(rest, "0123456789"))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "-"))

	desc, directive, _ := strings.Cut(rest, "#")
	c := Case{Name: strings.TrimSpace(desc), Status: Fail}
	if ok {
		c.Status = Pass
	}
	// SKIP is a case that did not run; TODO is one known to be broken, which
	// TAP says must not count as a failure. Neither is earned or lost, which
	// is exactly what Skip means for the score.
	if d := strings.ToUpper(strings.TrimSpace(directive)); strings.HasPrefix(d, "SKIP") || strings.HasPrefix(d, "TODO") {
		c.Status = Skip
		c.Message = strings.TrimSpace(directive)
	}
	if c.Name == "" {
		c.Name = fmt.Sprintf("test %d", seq)
	}
	return c
}

// addDetail appends an indented line to the message of the result it belongs
// to, up to the per-case bound - a YAML block is diagnostics, not a payload.
func addDetail(c *Case, line string) {
	if len(c.Message) >= MaxMessage {
		return
	}
	if c.Message != "" {
		c.Message += "\n"
	}
	c.Message += line
}
