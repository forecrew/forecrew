package advisor

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

// integrationWait is how long a tool may stay approved and not connected.
const integrationWait = 30 * 24 * time.Hour

// observabilityLibraries are the metrics, tracing and error-tracking
// libraries the detector knows, by component stack, as prefixes of a
// dependency's name. The generated dependency list covers go.mod and
// package.json.
var observabilityLibraries = map[string][]string{
	"go": {"go.opentelemetry.io/", "github.com/prometheus/client_golang", "github.com/getsentry/sentry-go", "github.com/DataDog/dd-trace-go",
		"gopkg.in/DataDog/dd-trace-go", "github.com/newrelic/go-agent", "github.com/rollbar/rollbar-go", "github.com/honeycombio/"},
	"node": {"@opentelemetry/", "prom-client", "@sentry/", "dd-trace", "newrelic", "rollbar", "@honeycombio/", "@bugsnag/"},
}

// detectObservability finds components of a known stack with none of the
// libraries and no monitoring integration approved or connected.
func detectObservability(d Data) []Signal {
	if d.Profile == nil || d.Dependencies == nil {
		return nil
	}
	for _, i := range d.Profile.Integrations {
		if slices.Contains(i.Uses, "monitoring") && (i.Status == "approved" || i.Status == "connected") {
			return nil
		}
	}
	var out []Signal
	for _, c := range d.Profile.Components {
		known, deps := observabilityLibraries[c.Stack], d.Dependencies[c.Path]
		if len(known) == 0 || len(deps) == 0 || slices.ContainsFunc(deps, func(dep string) bool {
			return slices.ContainsFunc(known, func(lib string) bool { return strings.HasPrefix(dep, lib) })
		}) {
			continue
		}
		out = append(out, Signal{Key: signalKey(DetectorObservability, c.Path), Category: domain.CategoryObservability, Subject: c.Path, Weight: 1,
			Summary:  fmt.Sprintf("Component %s has no metrics, tracing or error-tracking library.", c.Path),
			Evidence: []domain.ProposalEvidence{{Kind: DetectorObservability, Ref: c.Path, Detail: fmt.Sprintf("%d direct dependencies, none known for %s", len(deps), c.Stack)}}})
	}
	return out
}

// What in CI shows a scanner is at work.
var (
	dependencyScanners = []string{"dependabot", "renovate", "dependency-review-action", "govulncheck", "npm audit", "pnpm audit", "yarn audit",
		"pip-audit", "cargo audit", "cargo-audit", "osv-scanner", "trivy", "grype", "snyk"}
	secretScanners = []string{"gitleaks", "trufflehog", "detect-secrets", "ggshield"}
)

// ciMentions reports whether a CI file names one of words.
func ciMentions(files map[string]string, words []string) bool {
	for path, text := range files {
		all := strings.ToLower(path + "\n" + text)
		if slices.ContainsFunc(words, func(w string) bool { return strings.Contains(all, w) }) {
			return true
		}
	}
	return false
}

// detectSecurity finds a CI with no dependency or no secret scanning.
func detectSecurity(d Data) []Signal {
	if d.CIFiles == nil {
		return nil
	}
	evidence := []domain.ProposalEvidence{{Kind: DetectorSecurity, Ref: "ci", Detail: fmt.Sprintf("%d CI files read", len(d.CIFiles))}}
	var out []Signal
	if !ciMentions(d.CIFiles, dependencyScanners) {
		out = append(out, Signal{Key: "security:dependency-scanning", Category: domain.CategorySecurity, Weight: 1, Summary: "Nothing scans the dependencies in CI.", Evidence: evidence})
	}
	if !ciMentions(d.CIFiles, secretScanners) {
		out = append(out, Signal{Key: "security:secret-scanning", Category: domain.CategorySecurity, Weight: 1, Summary: "Nothing scans for secrets in CI.", Evidence: evidence})
	}
	return out
}

// runnerWords are what the runners of a platform are called.
var runnerWords = map[string][]string{
	"linux":   {"linux", "ubuntu", "debian"},
	"macos":   {"macos", "osx", "darwin"},
	"windows": {"windows"},
}

// detectCI finds a platform of the matrix that no CI runner builds. Only
// platforms with known runner names count, and a repo whose runners are
// unknown says nothing.
func detectCI(d Data) []Signal {
	if d.Profile == nil || len(d.Profile.CI.Runners) == 0 {
		return nil
	}
	runners := strings.ToLower(strings.Join(d.Profile.CI.Runners, " "))
	var out []Signal
	for _, p := range d.Platforms {
		words := runnerWords[strings.ToLower(p)]
		if len(words) == 0 || slices.ContainsFunc(words, func(w string) bool { return strings.Contains(runners, w) }) {
			continue
		}
		out = append(out, Signal{Key: signalKey("ci:platform", strings.ToLower(p)), Category: domain.CategoryCI, Subject: p, Weight: 1,
			Summary:  "The test matrix has " + p + ", but CI never builds it.",
			Evidence: []domain.ProposalEvidence{{Kind: DetectorCI, Ref: "ci", Detail: "runners: " + detail(strings.Join(d.Profile.CI.Runners, ", "))}}})
	}
	return out
}

// detectIntegrations finds tools approved in the profile and still not
// connected integrationWait after the profile's last approval, which is the
// only date a profile has. The weight grows with each further period.
func detectIntegrations(d Data) []Signal {
	if d.Profile == nil {
		return nil
	}
	var approved time.Time
	for _, g := range d.Gates {
		if g.Kind == domain.GateProfileApproval && g.Status == domain.GateApproved && g.ResolvedAt.After(approved) {
			approved = g.ResolvedAt
		}
	}
	if approved.IsZero() || d.Now.Sub(approved) < integrationWait {
		return nil
	}
	groups := map[string]*Signal{}
	for _, i := range d.Profile.Integrations {
		if i.Status != "approved" {
			continue
		}
		groups[i.Tool] = &Signal{Key: signalKey(DetectorIntegrations, i.Tool), Category: domain.CategoryTooling, Subject: i.Tool, Weight: float64(d.Now.Sub(approved) / integrationWait),
			Summary:  i.Tool + " is approved in the profile and not connected.",
			Evidence: []domain.ProposalEvidence{{Kind: DetectorIntegrations, Ref: i.Tool, Detail: "approved on " + approved.Format(time.DateOnly)}}}
	}
	return sorted(groups)
}
