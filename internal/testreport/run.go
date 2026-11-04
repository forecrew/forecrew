package testreport

import (
	"fmt"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// Names of the results added when a suite has no usable report or its
// command failed without a failing test.
const (
	CommandResult = "(command)"
	ReportResult  = "(report)"
	Classname     = "forecrew"
)

// CleanText removes what Postgres refuses in text: NUL bytes and invalid
// UTF-8.
func CleanText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
}

// Outcome is the status, counts and results of a test run.
type Outcome struct {
	Status  domain.TestRunStatus
	Counts  domain.TestCounts
	Results []Case
}

// Errored returns the outcome of a run that has no usable results: errored,
// with one result saying why. clean is applied to msg (nil: CleanText only).
func Errored(msg string, clean func(string) string) Outcome {
	if clean != nil {
		msg = clean(msg)
	}
	return Outcome{
		Status: domain.TestRunErrored, Counts: domain.TestCounts{Errored: 1},
		Results: []Case{{Name: ReportResult, Classname: Classname, Status: domain.TestErrored, Message: CleanText(msg)}},
	}
}

// Collector gathers the cases of the reports of one run within the limits of
// a run (MaxCases and MaxTotalSize), so that the memory a repo can make
// forecrewd or a worker use is bounded by the limits, not by its number of
// files.
type Collector struct {
	cases []Case
	size  int
	// Usable is how many reports parsed.
	Usable int
	// Problems say why the others did not.
	Problems []string
}

// Add records the parse result of the report called name. A report that did
// not parse is a problem and goes on; one that takes the run past the limits
// is the returned error, after which the collector must not be used.
func (c *Collector) Add(name string, parsed []Case, parseErr error) error {
	if parseErr != nil {
		c.Problems = append(c.Problems, fmt.Sprintf("report %s: %v", name, parseErr))
		return nil
	}
	c.size += Size(parsed)
	if c.cases = append(c.cases, parsed...); len(c.cases) > MaxCases || c.size > MaxTotalSize {
		return fmt.Errorf("the reports hold too many test cases: %w", Check(c.cases))
	}
	c.Usable++
	return nil
}

// Evaluation is what Evaluate needs to know about the run.
type Evaluation struct {
	// Declared are the report globs of the command; Found the report files
	// that matched them.
	Declared []string
	Found    int
	// ExitCode is the command's.
	ExitCode int
	// Clean is applied to every name and message (nil: CleanText only); a
	// worker redacts secrets with it.
	Clean func(string) string
}

// Evaluate turns the collected reports into the outcome of the run: errored
// when no report was declared, found or usable or when they hold no test
// case; otherwise the cases (and the reports that did not parse, and a
// non-zero exit code with no failing test, as errored results) summarized.
func (c *Collector) Evaluate(e Evaluation) Outcome {
	clean := func(s string) string {
		if e.Clean != nil {
			s = e.Clean(s)
		}
		return CleanText(s)
	}
	switch {
	case len(e.Declared) == 0:
		return Errored("the command declares no reports", clean)
	case e.Found == 0:
		return Errored("no report file matched "+strings.Join(e.Declared, ", "), clean)
	case c.Usable == 0:
		return Errored(strings.Join(c.Problems, "; "), clean)
	case len(c.cases) == 0:
		return Errored("the reports hold no test cases", clean)
	}
	cases := c.cases
	if err := Check(cases); err != nil {
		return Errored(err.Error(), clean)
	}
	for i := range cases {
		cases[i].Name, cases[i].Classname, cases[i].Message = clean(cases[i].Name), clean(cases[i].Classname), clean(cases[i].Message)
	}
	for _, p := range c.Problems {
		cases = append(cases, Case{Name: ReportResult, Classname: Classname, Status: domain.TestErrored, Message: clean(p)})
	}
	if e.ExitCode != 0 {
		// Tests that all passed under a failing command are not a pass.
		if _, status := Summarize(cases); status == domain.TestRunPassed {
			cases = append(cases, Case{Name: CommandResult, Classname: Classname, Status: domain.TestErrored,
				Message: fmt.Sprintf("the command exited with code %d although no test failed", e.ExitCode)})
		}
	}
	counts, status := Summarize(cases)
	return Outcome{Status: status, Counts: counts, Results: cases}
}
