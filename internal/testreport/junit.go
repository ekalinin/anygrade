package testreport

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// junitCase is the <testcase> element of the JUnit/xUnit XML schema. Only the
// parts every generator agrees on are modelled; anything else in the document
// is skipped by the walk below.
type junitCase struct {
	Name string `xml:"name,attr"`
	// Time is text rather than a float: a locale-formatted or empty attribute
	// would otherwise fail the whole element, and a missing duration is worth
	// far less than the verdict beside it.
	Time     string        `xml:"time,attr"`
	Failures []junitDetail `xml:"failure"`
	Errors   []junitDetail `xml:"error"`
	Skipped  *junitDetail  `xml:"skipped"`
}

type junitDetail struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// parseJUnit reads JUnit XML.
//
// The document shape is not modelled at all: a report may be a <testsuites>
// root, a bare <testsuite>, or suites nested inside suites, and every
// generator picks one. Walking the token stream and decoding each <testcase>
// wherever it appears reads all three without a schema, and a malformed
// document fails as a whole - which is right for a format that has no partial
// reading: half an XML tree says nothing about how many tests there were.
func parseJUnit(data []byte) ([]Case, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var cases []Case
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return cases, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "testcase" {
			continue
		}
		var tc junitCase
		if err := dec.DecodeElement(&tc, &se); err != nil {
			return nil, err
		}
		if len(cases) >= MaxCases {
			return nil, ErrTooManyCases
		}
		cases = append(cases, tc.result())
	}
}

// result maps one <testcase> to a verdict. An <error> - the runner blew up
// rather than the assertion - is a failure like any other: whatever it means,
// the case did not pass.
func (tc junitCase) result() Case {
	c := Case{Name: tc.Name, Status: Pass, Duration: parseSeconds(tc.Time)}
	switch {
	case len(tc.Failures) > 0:
		c.Status, c.Message = Fail, tc.Failures[0].text()
	case len(tc.Errors) > 0:
		c.Status, c.Message = Fail, tc.Errors[0].text()
	case tc.Skipped != nil:
		c.Status, c.Message = Skip, tc.Skipped.text()
	}
	return c
}

// text joins the summary attribute with the element body; generators use one,
// the other, or both.
func (d junitDetail) text() string {
	parts := make([]string, 0, 2)
	for _, s := range []string{d.Message, d.Text} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}
