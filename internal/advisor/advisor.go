// Package advisor finds what a project, or Forecrew's setup for it, should
// gain (decision 24). Detectors turn data Forecrew already has into signals;
// the advisor role turns the signals into a few proposals; Service files
// them, follows the human's decision and measures the result.
package advisor

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/verification"
)

// Detector names; a proposal's measure names one.
const (
	DetectorWorkers       = "workers"
	DetectorTooling       = "tooling"
	DetectorKnowledge     = "knowledge"
	DetectorTesting       = "testing"
	DetectorRework        = "rework"
	DetectorCost          = "cost"
	DetectorObservability = "observability"
	DetectorSecurity      = "security"
	DetectorCI            = "ci"
	DetectorIntegrations  = "integrations"
)

// maxEvidence bounds the evidence of a signal, and so of a proposal.
const maxEvidence = 10

// Signal is one thing a detector found. Key is stable from one run to the
// next, so it serves as the dedupe key of the proposal that answers it.
type Signal struct {
	Detector string                  `json:"detector"`
	Key      string                  `json:"key"`
	Category domain.ProposalCategory `json:"category"`
	Summary  string                  `json:"summary"`
	// Subject is what the signal is about, for the tooling detector the
	// missing tool. It may come from an agent's text.
	Subject string `json:"subject,omitempty"`
	// Weight grows with the evidence; a proposal's before and after values
	// are its signal's weight, zero once the signal is gone.
	Weight   float64                   `json:"weight"`
	Evidence []domain.ProposalEvidence `json:"evidence"`
}

// Report is a verification report with the PR it belongs to.
type Report struct {
	Ref    string              `json:"ref"` // PR #12
	Report verification.Report `json:"report"`
}

// Data is what the detectors read, gathered by Service.Collect.
type Data struct {
	Now     time.Time         `json:"now"`
	Jobs    []domain.Job      `json:"jobs"`
	Workers []domain.Worker   `json:"workers"`
	Gates   []domain.Gate     `json:"gates"`
	Changes []domain.Change   `json:"changes"`
	Reports []Report          `json:"reports"`
	Profile *analyzer.Profile `json:"profile"` // nil before onboarding
	// Dependencies are the direct dependencies by component path, from the
	// generated knowledge section; nil when it was not built yet.
	Dependencies map[string][]string `json:"dependencies"`
	// CIFiles holds the workflow files of the profile and the update-bot
	// configs that exist, by path; nil when the repo could not be read.
	CIFiles map[string]string `json:"ci_files"`
	// Platforms are the platforms of the test matrix, else of the profile.
	Platforms []string `json:"platforms"`
}

// detectors are all the detectors by name, in the order they run.
var detectors = []struct {
	name   string
	detect func(Data) []Signal
}{
	{DetectorWorkers, detectWorkers},
	{DetectorTooling, detectTooling},
	{DetectorKnowledge, detectKnowledge},
	{DetectorTesting, detectTesting},
	{DetectorRework, detectRework},
	{DetectorCost, detectCost},
	{DetectorObservability, detectObservability},
	{DetectorSecurity, detectSecurity},
	{DetectorCI, detectCI},
	{DetectorIntegrations, detectIntegrations},
}

// Detect runs every detector and returns its signals, heaviest first.
func Detect(d Data) []Signal {
	var out []Signal
	for _, det := range detectors {
		for _, s := range det.detect(d) {
			s.Detector = det.name
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b Signal) int {
		return cmp.Or(cmp.Compare(b.Weight, a.Weight), cmp.Compare(a.Key, b.Key))
	})
	return out
}

// find returns the signal with key.
func find(signals []Signal, key string) (Signal, bool) {
	i := slices.IndexFunc(signals, func(s Signal) bool { return s.Key == key })
	if i < 0 {
		return Signal{}, false
	}
	return signals[i], true
}

// Bounds of a signal key: a subject longer than maxKeySubject keeps its
// first keptSubject bytes and a hash of the whole, so the key stays short,
// stable from one run to the next, and distinct.
const (
	maxKeySubject = 64
	keptSubject   = 40
)

// signalKey is the key of a signal about subject, which may be repo or
// agent text: a test name, a path, labels.
func signalKey(prefix, subject string) string {
	if len(subject) <= maxKeySubject {
		return prefix + ":" + subject
	}
	sum := sha256.Sum256([]byte(subject))
	return prefix + ":" + strings.ToValidUTF8(subject[:keptSubject], "") + "~" + hex.EncodeToString(sum[:6])
}

// bounded cuts evidence to maxEvidence items.
func bounded(e []domain.ProposalEvidence) []domain.ProposalEvidence {
	if len(e) > maxEvidence {
		return e[:maxEvidence]
	}
	return e
}
