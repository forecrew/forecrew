package testreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestParseGolden(t *testing.T) {
	for _, name := range []string{"gotestsum", "jest", "pytest", "surefire"} {
		t.Run(name, func(t *testing.T) {
			got := parseFixture(t, name)
			golden := filepath.Join("testdata", name+".golden.json")
			if *update {
				if err := os.WriteFile(golden, append(got, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(want)) != string(got) {
				t.Errorf("cases differ from %s:\n%s", golden, got)
			}
		})
	}
}

// parseFixture returns the parsed cases of testdata/<name>.xml as JSON.
func parseFixture(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".xml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cases, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestParseBrokenFiles(t *testing.T) {
	for name, want := range map[string]string{
		"broken_truncated.xml": "unexpected EOF",
		"broken_html.xml":      "root element is <html>",
	} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			cases, err := Parse(bytes.NewReader(b))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) || cases != nil {
				t.Errorf("Parse = %v, %v; want ErrInvalid containing %q", cases, err, want)
			}
		})
	}
}

func TestParseCases(t *testing.T) {
	tests := []struct {
		name string
		xml  string
		want []Case
	}{
		{"nested suites", `<testsuites><testsuite name="a"><testsuite name="b"><testcase name="deep" classname="c" time="1.5"/></testsuite><testcase name="shallow"/></testsuite></testsuites>`,
			[]Case{{Name: "deep", Classname: "c", Status: domain.TestPassed, DurationMS: 1500}, {Name: "shallow", Status: domain.TestPassed}}},
		{"error beats failure", `<testsuite><testcase name="t"><failure message="f"/><error message="e"/></testcase></testsuite>`,
			[]Case{{Name: "t", Status: domain.TestErrored, Message: "e"}}},
		{"failure beats skipped", `<testsuite><testcase name="t"><skipped/><failure message="f"/></testcase></testsuite>`,
			[]Case{{Name: "t", Status: domain.TestFailed, Message: "f"}}},
		{"message and text", `<testsuite><testcase name="t"><failure message="short">long text</failure></testcase><testcase name="u"><failure message="same">same</failure></testcase></testsuite>`,
			[]Case{{Name: "t", Status: domain.TestFailed, Message: "short\nlong text"}, {Name: "u", Status: domain.TestFailed, Message: "same"}}},
		{"unnamed case", `<testsuite><testcase classname="c"/></testsuite>`, []Case{{Name: "(unnamed)", Classname: "c", Status: domain.TestPassed}}},
		{"empty suite", `<testsuites/>`, nil},
		{"junk children are skipped", `<testsuite><properties><property name="a" value="b"/></properties><system-out>x</system-out><testcase name="t"><system-out>log</system-out><rerunFailure message="flaky"/></testcase></testsuite>`,
			[]Case{{Name: "t", Status: domain.TestPassed}}},
		{"bad times", `<testsuite><testcase name="a" time="NaN"/><testcase name="b" time="-3"/><testcase name="c" time="x"/><testcase name="d" time="0.0004"/></testsuite>`,
			[]Case{{Name: "a", Status: domain.TestPassed}, {Name: "b", Status: domain.TestPassed}, {Name: "c", Status: domain.TestPassed}, {Name: "d", Status: domain.TestPassed}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tt.xml))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("case %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	huge := "<testsuite>" + strings.Repeat(" ", MaxReportSize) + "</testsuite>"
	for name, in := range map[string]string{
		"empty":            "",
		"text":             "not xml at all",
		"only whitespace":  "  \n ",
		"unclosed case":    `<testsuite><testcase name="a">`,
		"bad nested":       `<testsuite><testcase name="a"><failure></testcase></testsuite>`,
		"bad skipped body": `<testsuite><properties></testsuite>`,
		"too large":        huge,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(in)); !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk gone") }

func TestParseReadError(t *testing.T) {
	if _, err := Parse(failingReader{}); err == nil || errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want a read error", err)
	}
}

func TestMessageIsClipped(t *testing.T) {
	long := strings.Repeat("é", MaxMessageSize)
	cases, err := Parse(strings.NewReader(`<testsuite><testcase name="t"><failure message="` + long + `"/></testcase></testsuite>`))
	if err != nil {
		t.Fatal(err)
	}
	msg := cases[0].Message
	if len(msg) > MaxMessageSize+len("... [truncated]") || !strings.HasSuffix(msg, "... [truncated]") {
		t.Errorf("message has %d bytes: ...%q", len(msg), msg[len(msg)-20:])
	}
	if strings.ToValidUTF8(msg, "") != msg {
		t.Error("message was cut inside a rune")
	}
}

func TestSummarize(t *testing.T) {
	tests := []struct {
		name   string
		cases  []Case
		counts domain.TestCounts
		status domain.TestRunStatus
	}{
		{"none", nil, domain.TestCounts{}, domain.TestRunPassed},
		{"pass and skip", []Case{{Status: domain.TestPassed}, {Status: domain.TestSkipped}}, domain.TestCounts{Passed: 1, Skipped: 1}, domain.TestRunPassed},
		{"failure", []Case{{Status: domain.TestPassed}, {Status: domain.TestFailed}}, domain.TestCounts{Passed: 1, Failed: 1}, domain.TestRunFailed},
		{"error", []Case{{Status: domain.TestErrored}}, domain.TestCounts{Errored: 1}, domain.TestRunFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts, status := Summarize(tt.cases)
			if counts != tt.counts || status != tt.status {
				t.Errorf("Summarize = %+v, %s; want %+v, %s", counts, status, tt.counts, tt.status)
			}
		})
	}
}

func TestDurationsAreClamped(t *testing.T) {
	cases, err := Parse(strings.NewReader(`<testsuite><testcase name="a" time="1e16"/><testcase name="b" time="9223372036854775807"/><testcase name="c" time="1e400"/></testsuite>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.DurationMS < 0 || c.DurationMS > maxDurationMS {
			t.Errorf("%s: duration %d", c.Name, c.DurationMS)
		}
	}
	if cases[0].DurationMS != maxDurationMS {
		t.Errorf("1e16 s = %d ms, want the cap", cases[0].DurationMS)
	}
}

func TestNamesAreClipped(t *testing.T) {
	long := strings.Repeat("n", 10*MaxNameSize)
	cases, err := Parse(strings.NewReader(`<testsuite><testcase name="` + long + `" classname="` + long + `"/></testsuite>`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases[0].Name) > MaxNameSize+len("... [truncated]") || len(cases[0].Classname) > MaxNameSize+len("... [truncated]") {
		t.Errorf("name %d bytes, classname %d bytes", len(cases[0].Name), len(cases[0].Classname))
	}
}

func TestReportBounds(t *testing.T) {
	many := "<testsuite>" + strings.Repeat("<testcase name=\"t\"/>", MaxCases+1) + "</testsuite>"
	if _, err := Parse(strings.NewReader(many)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "test cases") {
		t.Errorf("too many cases: %v", err)
	}
	msg := strings.Repeat("m", MaxMessageSize)
	fat := "<testsuite>" + strings.Repeat(`<testcase name="t"><failure message="`+msg+`"/></testcase>`, MaxTotalSize/MaxMessageSize+10) + "</testsuite>"
	if _, err := Parse(strings.NewReader(fat)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("too many bytes: %v", err)
	}
	if err := Check([]Case{{Name: "a"}}); err != nil {
		t.Errorf("Check of a small run: %v", err)
	}
}
