package advisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func fixture(t *testing.T, name string) Data {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

// Each detector on a stored fixture, and on one just below its threshold.
func TestDetectors(t *testing.T) {
	cases := []struct {
		fixture string
		want    map[string]float64 // key -> weight
	}{
		{"workers", map[string]float64{"workers:build:macos,runtime:claude-code": 2}},
		{"tooling", map[string]float64{"tooling:terraform": 2}},
		{"knowledge", map[string]float64{"knowledge:connect-database-integration": 2}},
		{"testing", map[string]float64{"testing:untested": 2, "testing:flaky:TestLogin": 3, "testing:incomplete:linux/e2e": 3}},
		{"rework", map[string]float64{"rework:error-handling": 3}},
		{"cost", map[string]float64{"cost:reviewer": 6}},
		{"observability", map[string]float64{"observability:.": 1}},
		{"security", map[string]float64{"security:dependency-scanning": 1, "security:secret-scanning": 1}},
		{"ci", map[string]float64{"ci:platform:windows": 1}},
		{"integrations", map[string]float64{"integrations:jira": 1}},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			detect := detectorNamed(t, c.fixture)
			if below := detect(fixture(t, c.fixture+"-below")); len(below) != 0 {
				t.Errorf("below the threshold: %+v", below)
			}
			checkSignals(t, detect(fixture(t, c.fixture)), c.want)
		})
	}
}

func detectorNamed(t *testing.T, name string) func(Data) []Signal {
	t.Helper()
	for _, det := range detectors {
		if det.name == name {
			return det.detect
		}
	}
	t.Fatalf("no detector %s", name)
	return nil
}

func checkSignals(t *testing.T, got []Signal, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %v", got, want)
	}
	for _, s := range got {
		if w, ok := want[s.Key]; !ok || w != s.Weight {
			t.Errorf("%s weighs %v, want %v in %v", s.Key, s.Weight, w, want)
		}
		if s.Category == "" || s.Summary == "" || len(s.Evidence) == 0 {
			t.Errorf("incomplete signal: %+v", s)
		}
	}
}

func TestDetectRunsAllAndSorts(t *testing.T) {
	d := fixture(t, "testing")
	d.CIFiles = fixture(t, "security").CIFiles
	got := Detect(d)
	if len(got) != 5 || got[0].Key != "testing:flaky:TestLogin" || got[0].Detector != DetectorTesting || got[len(got)-1].Detector != DetectorSecurity {
		t.Fatalf("got %+v", got)
	}
	if len(Detect(Data{})) != 0 {
		t.Error("no data must give no signals")
	}
}

func TestMissingTools(t *testing.T) {
	cases := map[string][]string{
		"sh: 1: psql: command not found":                     {"psql"},
		"zsh: command not found: Helm":                       {"helm"},
		`exec: "docker": executable file not found in $PATH`: {"docker"},
		"`terraform` is not installed":                       {"terraform"},
		"missing tool: ffmpeg":                               {"ffmpeg"},
		"I have no access to Vault":                          {"vault"},
		"could not connect to redis on port 6379":            {"redis"},
		"this job requires a stripe service":                 {"stripe"},
		"it is not installed":                                nil,
		"everything went fine":                               nil,
		strings.Repeat("x", 41) + ": command not found":      nil,
	}
	for in, want := range cases {
		if got := missingTools(in); !slices.Equal(got, want) {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

func TestThemeOf(t *testing.T) {
	for body, want := range map[string]string{"SQL injection here": "security", "a data race on cache": "concurrency", "add a test for this": "tests",
		"please rename": "naming", "this already exists in util": "duplication", "document this": "docs", "add logging": "logging", "odd": ""} {
		if got := themeOf(body); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want float64
	}{{nil, 0}, {[]float64{3}, 3}, {[]float64{4, 1}, 2.5}, {[]float64{9, 1, 2}, 2}} {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v", c.in, got)
		}
	}
}

func TestParseDependencies(t *testing.T) {
	got := parseDependencies("## .\n\n- github.com/a/b v1.2.3 (go)\n\n## web\n\n- react 18 (npm)\n- vitest 1 (npm-dev)\n")
	if !slices.Equal(got["."], []string{"github.com/a/b"}) || !slices.Equal(got["web"], []string{"react", "vitest"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSignalKeysAreBounded(t *testing.T) {
	long := "Test" + strings.Repeat("Ä", 60)
	k := signalKey("testing:flaky", long)
	if len(k) > len("testing:flaky:")+keptSubject+13 || !utf8.ValidString(k) || !strings.HasPrefix(k, "testing:flaky:Test") {
		t.Fatalf("key %q", k)
	}
	if k != signalKey("testing:flaky", long) || k == signalKey("testing:flaky", long+"x") {
		t.Fatal("a long key must be stable and distinct")
	}
	if got := signalKey("cost", "reviewer"); got != "cost:reviewer" {
		t.Fatalf("a short key = %q", got)
	}
	d := fixture(t, "testing")
	for i := range d.Reports {
		for j := range d.Reports[i].Report.Flaky {
			d.Reports[i].Report.Flaky[j].Test = long
		}
	}
	for _, s := range detectTesting(d) {
		if len(s.Key) > 80 {
			t.Errorf("key %q", s.Key)
		}
	}
}

// A cluster whose first questions share no word has no topic, and the
// detector refuses it rather than file a signal keyed "knowledge:".
func TestTopicOf(t *testing.T) {
	q := func(words ...string) question {
		m := map[string]bool{}
		for _, w := range words {
			m[w] = true
		}
		return question{terms: m}
	}
	if got := topicOf([]question{q("staging", "database", "tests"), q("database", "staging", "url")}); got != "database-staging" {
		t.Fatalf("topic %q", got)
	}
	if got := topicOf([]question{q("staging"), q("logo")}); got != "" {
		t.Fatalf("topic %q", got)
	}
}
