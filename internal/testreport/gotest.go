package testreport

import (
	"bytes"
	"encoding/json"
	"strings"
)

// goEvent is the part of a `go test -json` event this parser reads. The stream
// is one JSON object per line; Test is empty on the package-level events,
// which are not test cases.
type goEvent struct {
	Action  string  `json:"Action"`  // run | pause | cont | output | pass | fail | skip
	Test    string  `json:"Test"`    // empty for package-level events
	Elapsed float64 `json:"Elapsed"` // seconds, on the terminal event
	Output  string  `json:"Output"`
}

// parseGoTest reads `go test -json`.
//
// Lines that are not JSON objects are skipped rather than refused: a check's
// stdout and stderr share one stream, so `go: downloading ...`, a compiler
// error or a stray print from the solution sits in the middle of an otherwise
// perfectly good report. A stream that is nothing but such lines yields no
// case at all, which is how genuinely unparseable output reaches the caller.
func parseGoTest(data []byte) ([]Case, error) {
	var cases []Case
	index := map[string]int{} // test name -> position in cases

	for line := range bytes.Lines(data) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev goEvent
		if err := json.Unmarshal(line, &ev); err != nil || ev.Test == "" {
			continue
		}
		i, seen := index[ev.Test]
		if !seen {
			if len(cases) >= MaxCases {
				return nil, ErrTooManyCases
			}
			i = len(cases)
			index[ev.Test] = i
			// Status stays empty until a terminal event arrives: a test the
			// stream never finished (a panic took the binary down under it)
			// has no verdict to report and is dropped below.
			cases = append(cases, Case{Name: ev.Test})
		}
		c := &cases[i]
		switch ev.Action {
		case "output":
			if len(c.Message) < MaxMessage {
				c.Message += testOutput(ev.Output)
			}
		case "pass":
			c.Status, c.Duration = Pass, seconds(ev.Elapsed)
		case "fail":
			c.Status, c.Duration = Fail, seconds(ev.Elapsed)
		case "skip":
			c.Status, c.Duration = Skip, seconds(ev.Elapsed)
		}
	}
	return dropParents(finished(cases)), nil
}

// testOutput drops test2json's own framing lines ("=== RUN", "--- FAIL: ...")
// from the text kept as a case's message: they repeat the name and the verdict
// the row already carries, and the message budget is small.
func testOutput(s string) string {
	if strings.HasPrefix(s, "=== ") || strings.HasPrefix(s, "--- ") {
		return ""
	}
	return s
}

// finished keeps the cases that reached a terminal action. Anything else is a
// test that started and never reported, which is not a failure to score - it
// is a report that stops mid-sentence.
func finished(cases []Case) []Case {
	out := cases[:0]
	for _, c := range cases {
		if c.Status != "" {
			out = append(out, c)
		}
	}
	return out
}

// dropParents removes a test that is only the parent of its subtests. `go test
// -json` reports TestFoo and TestFoo/case-1 alike, so counting both would make
// a test with three subtests worth four cases and would charge one failing
// subtest twice - to itself and to the parent it fails.
func dropParents(cases []Case) []Case {
	parents := make(map[string]bool, len(cases))
	for _, c := range cases {
		// Every "/"-delimited prefix of a subtest name is a parent name.
		for i := strings.IndexByte(c.Name, '/'); i >= 0; {
			parents[c.Name[:i]] = true
			j := strings.IndexByte(c.Name[i+1:], '/')
			if j < 0 {
				break
			}
			i += 1 + j
		}
	}
	out := cases[:0]
	for _, c := range cases {
		if !parents[c.Name] {
			out = append(out, c)
		}
	}
	return out
}
