package testreport

import (
	"errors"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

func TestCleanText(t *testing.T) {
	if got := CleanText("a\x00b\xffc"); got != "abc" {
		t.Errorf("CleanText = %q", got)
	}
}

func TestErrored(t *testing.T) {
	o := Errored("bad\x00 thing", strings.ToUpper)
	if o.Status != domain.TestRunErrored || o.Counts != (domain.TestCounts{Errored: 1}) || len(o.Results) != 1 ||
		o.Results[0].Message != "BAD THING" || o.Results[0].Name != ReportResult || o.Results[0].Classname != Classname {
		t.Errorf("outcome = %+v", o)
	}
	if got := Errored("x", nil).Results[0].Message; got != "x" {
		t.Errorf("message = %q", got)
	}
}

func passed(name string) Case  { return Case{Name: name, Status: domain.TestPassed} }
func failedc(name string) Case { return Case{Name: name, Status: domain.TestFailed, Message: "boom"} }

func TestCollectorEvaluate(t *testing.T) {
	tests := []struct {
		name     string
		declared []string
		found    int
		add      func(c *Collector)
		exit     int
		status   domain.TestRunStatus
		counts   domain.TestCounts
		message  string // of the last result
	}{
		{"no reports declared", nil, 0, func(*Collector) {}, 0, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "the command declares no reports"},
		{"no file matched", []string{"r/*.xml"}, 0, func(*Collector) {}, 0, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "no report file matched r/*.xml"},
		{"none parsed", []string{"r.xml"}, 1, func(c *Collector) { _ = c.Add("r.xml", nil, errors.New("bad xml")) }, 0,
			domain.TestRunErrored, domain.TestCounts{Errored: 1}, "report r.xml: bad xml"},
		{"no cases", []string{"r.xml"}, 1, func(c *Collector) { _ = c.Add("r.xml", nil, nil) }, 0, domain.TestRunErrored, domain.TestCounts{Errored: 1}, "the reports hold no test cases"},
		{"passed", []string{"r.xml"}, 1, func(c *Collector) { _ = c.Add("r.xml", []Case{passed("a")}, nil) }, 0, domain.TestRunPassed, domain.TestCounts{Passed: 1}, ""},
		{"failed", []string{"r.xml"}, 1, func(c *Collector) { _ = c.Add("r.xml", []Case{passed("a"), failedc("b")}, nil) }, 1, domain.TestRunFailed, domain.TestCounts{Passed: 1, Failed: 1}, "boom"},
		{"one report broken", []string{"r*.xml"}, 2, func(c *Collector) {
			_ = c.Add("r1.xml", []Case{passed("a")}, nil)
			_ = c.Add("r2.xml", nil, errors.New("bad xml"))
		}, 0, domain.TestRunFailed, domain.TestCounts{Passed: 1, Errored: 1}, "report r2.xml: bad xml"},
		{"exit code without failure", []string{"r.xml"}, 1, func(c *Collector) { _ = c.Add("r.xml", []Case{passed("a")}, nil) }, 2,
			domain.TestRunFailed, domain.TestCounts{Passed: 1, Errored: 1}, "the command exited with code 2 although no test failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Collector
			tt.add(&c)
			o := c.Evaluate(Evaluation{Declared: tt.declared, Found: tt.found, ExitCode: tt.exit})
			if o.Status != tt.status || o.Counts != tt.counts {
				t.Fatalf("outcome = %+v", o)
			}
			if last := o.Results[len(o.Results)-1].Message; last != tt.message {
				t.Errorf("last message = %q, want %q", last, tt.message)
			}
		})
	}
}

func TestCollectorLimits(t *testing.T) {
	var c Collector
	big := make([]Case, MaxCases)
	for i := range big {
		big[i] = passed("t")
	}
	if err := c.Add("a.xml", big, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Add("b.xml", []Case{passed("one more")}, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestEvaluateCleansAndChecks(t *testing.T) {
	var c Collector
	_ = c.Add("r.xml", []Case{{Name: "a\x00", Classname: "k", Status: domain.TestPassed, Message: "secret-value"}}, nil)
	o := c.Evaluate(Evaluation{Declared: []string{"r.xml"}, Found: 1, Clean: func(s string) string { return strings.ReplaceAll(s, "secret-value", "[redacted]") }})
	if o.Results[0].Name != "a" || o.Results[0].Message != "[redacted]" {
		t.Errorf("results = %+v", o.Results)
	}
	c = Collector{cases: make([]Case, MaxCases+1), Usable: 1}
	if o := c.Evaluate(Evaluation{Declared: []string{"r"}, Found: 1}); o.Status != domain.TestRunErrored {
		t.Errorf("too many cases: %+v", o)
	}
}
