// Package testreport parses JUnit XML reports, the format every test
// runner can write, into test cases. The variants of gotestsum, Jest,
// pytest and Maven Surefire are tested against real-looking fixtures.
package testreport

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/forecrew/forecrew/internal/domain"
)

// Limits of a report.
const (
	// MaxReportSize is the largest report that is parsed.
	MaxReportSize = 32 << 20
	// MaxMessageSize is the most bytes of a failure message that are kept.
	MaxMessageSize = 4096
	// MaxNameSize is the most bytes of a test name or class name that are
	// kept.
	MaxNameSize = 512
	// MaxCases and MaxTotalSize bound the cases of a run (all its reports)
	// and the bytes of their names and messages; see Check.
	MaxCases     = 100_000
	MaxTotalSize = 16 << 20
	// maxDurationMS caps a case's duration (a year), so a time attribute
	// such as 1e16 cannot overflow.
	maxDurationMS = 366 * 24 * 3600 * 1000
)

// ErrInvalid means the input is not a usable JUnit report.
var ErrInvalid = errors.New("invalid JUnit report")

// Case is one test case of a report.
type Case struct {
	Name       string            `json:"name"`
	Classname  string            `json:"classname"`
	Status     domain.TestStatus `json:"status"`
	DurationMS int64             `json:"duration_ms"`
	// Message is the text of the failure, error or skip; empty for a pass.
	Message string `json:"message"`
}

// Size is the bytes of the names and messages of cases, which Check bounds.
func Size(cases []Case) int {
	size := 0
	for _, c := range cases {
		size += len(c.Name) + len(c.Classname) + len(c.Message)
	}
	return size
}

// Check fails, wrapping ErrInvalid, when cases are more than MaxCases or hold
// more than MaxTotalSize bytes of names and messages.
func Check(cases []Case) error {
	if len(cases) > MaxCases {
		return fmt.Errorf("%w: more than %d test cases", ErrInvalid, MaxCases)
	}
	if Size(cases) > MaxTotalSize {
		return fmt.Errorf("%w: test names and messages over %d bytes", ErrInvalid, MaxTotalSize)
	}
	return nil
}

// Parse reads a JUnit XML report: a <testsuites> or <testsuite> root with
// nested suites and <testcase> elements. A case with an <error> is errored,
// with a <failure> failed, with a <skipped> skipped, and passed otherwise.
// Malformed XML, another root element or a report over MaxReportSize is an
// error wrapping ErrInvalid, as is one that fails Check. Names are cut to
// MaxNameSize and messages to MaxMessageSize.
func Parse(r io.Reader) ([]Case, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxReportSize+1))
	if err != nil {
		return nil, fmt.Errorf("read report: %w", err)
	}
	if len(data) > MaxReportSize {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrInvalid, MaxReportSize)
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var cases []Case
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			var err error
			if cases, depth, err = start(dec, t, depth, cases); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
		case xml.EndElement:
			depth--
		}
	}
	if !seenRoot(data) || depth != 0 {
		return nil, fmt.Errorf("%w: no <testsuites> or <testsuite> element", ErrInvalid)
	}
	if err := Check(cases); err != nil {
		return nil, err
	}
	return cases, nil
}

// start handles a start element at depth and returns the cases so far and
// the new depth. It fails when the cases outgrow MaxCases.
func start(dec *xml.Decoder, t xml.StartElement, depth int, cases []Case) ([]Case, int, error) {
	c, descend, err := element(dec, t, depth)
	if err != nil {
		return cases, depth, err
	}
	if c != nil {
		if cases = append(cases, *c); len(cases) > MaxCases {
			return cases, depth, Check(cases)
		}
	}
	if descend {
		depth++
	}
	return cases, depth, nil
}

// element handles a start element at depth: a test case is read to its end,
// a suite is descended into, anything else is skipped.
func element(dec *xml.Decoder, t xml.StartElement, depth int) (c *Case, descend bool, err error) {
	if depth == 0 && t.Name.Local != "testsuites" && t.Name.Local != "testsuite" {
		return nil, false, fmt.Errorf("root element is <%s>, want <testsuites> or <testsuite>", t.Name.Local)
	}
	switch t.Name.Local {
	case "testsuites", "testsuite":
		return nil, true, nil
	case "testcase":
		got, err := readCase(dec, t)
		return &got, false, err
	}
	return nil, false, dec.Skip() // properties, system-out, ...
}

// seenRoot reports whether data holds a suite element at all; text without
// any element parses without error.
func seenRoot(data []byte) bool {
	return bytes.Contains(data, []byte("<testsuite"))
}

// outcome is a <failure>, <error> or <skipped> element.
type outcome struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

func (o outcome) message() string {
	msg, text := strings.TrimSpace(o.Message), strings.TrimSpace(o.Text)
	switch {
	case msg == "":
		msg = text
	case text != "" && text != msg:
		msg += "\n" + text
	}
	return clip(msg, MaxMessageSize)
}

// readCase reads one <testcase> whose start element was just read.
func readCase(dec *xml.Decoder, start xml.StartElement) (Case, error) {
	c := Case{Status: domain.TestPassed}
	for _, a := range start.Attr {
		switch a.Name.Local {
		case "name":
			c.Name = clip(a.Value, MaxNameSize)
		case "classname":
			c.Classname = clip(a.Value, MaxNameSize)
		case "time":
			c.DurationMS = millis(a.Value)
		}
	}
	var failure, errored, skipped *outcome
	for {
		tok, err := dec.Token()
		if err != nil {
			return c, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			var o outcome
			switch t.Name.Local {
			case "failure":
				err = dec.DecodeElement(&o, &t)
				failure = &o
			case "error":
				err = dec.DecodeElement(&o, &t)
				errored = &o
			case "skipped":
				err = dec.DecodeElement(&o, &t)
				skipped = &o
			default: // system-out, system-err, properties, rerunFailure, ...
				err = dec.Skip()
			}
			if err != nil {
				return c, err
			}
		case xml.EndElement:
			switch {
			case errored != nil:
				c.Status, c.Message = domain.TestErrored, errored.message()
			case failure != nil:
				c.Status, c.Message = domain.TestFailed, failure.message()
			case skipped != nil:
				c.Status, c.Message = domain.TestSkipped, skipped.message()
			}
			if c.Name == "" {
				c.Name = "(unnamed)"
			}
			return c, nil
		}
	}
}

// millis converts a time attribute in seconds ("0.012", "1,234.5") to
// milliseconds; anything unreadable is 0.
func millis(s string) int64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0
	}
	return int64(min(math.Round(f*1000), maxDurationMS))
}

// clip cuts s to max bytes at a rune boundary.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	n := max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "... [truncated]"
}

// Summarize counts the cases by outcome and returns the status of the run
// they make: failed when a case failed or errored, otherwise passed.
func Summarize(cases []Case) (domain.TestCounts, domain.TestRunStatus) {
	var n domain.TestCounts
	for _, c := range cases {
		switch c.Status {
		case domain.TestPassed:
			n.Passed++
		case domain.TestFailed:
			n.Failed++
		case domain.TestSkipped:
			n.Skipped++
		case domain.TestErrored:
			n.Errored++
		}
	}
	if n.Failed+n.Errored > 0 {
		return n, domain.TestRunFailed
	}
	return n, domain.TestRunPassed
}
