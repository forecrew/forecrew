package playbooks_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/testkit"
)

// rateLimited is a tracker whose autolinks read is rate limited.
type rateLimited struct{ *testkit.Tracker }

func (rateLimited) ListAutolinks(context.Context, string) ([]github.Autolink, error) {
	return nil, github.ErrRateLimited
}

// discoverWithGitHubTools returns an env whose repo has an autolink to Jira,
// an issue linking Sentry and a pull request linking Linear.
func discoverWithGitHubTools(t *testing.T) *env {
	t.Helper()
	v := newEnv(t)
	ctx := t.Context()
	must(t, v.tracker.AddAutolink("acme/app", github.Autolink{KeyPrefix: "ENG-", URLTemplate: "https://acme.atlassian.net/browse/ENG-<num>"}))
	_, err := v.tracker.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "Crash", Body: "Errors: https://acme.sentry.io/issues/7"})
	must(t, err)
	_, err = v.tracker.CreatePR(ctx, "acme/app", github.PRInput{Title: "Plan", Body: "Spec: https://linear.app/acme/issue/ENG-1", Head: "agent/plan", Base: "main"})
	must(t, err)
	return v
}

func TestDiscoverGitHub(t *testing.T) {
	v := discoverWithGitHubTools(t)
	out, err := v.call(t, "discover.github", map[string]any{"repo": "acme/app"})
	must(t, err)
	m := toMap(t, out)
	ints, _ := m["integrations"].([]any)
	if len(ints) != 3 {
		t.Fatalf("integrations = %v, want jira, sentry and linear", m["integrations"])
	}
	text, _ := m["text"].(string)
	for _, want := range []string{"## Tools found on GitHub", `"tool": "jira"`, `"confidence": "high"`, `"tool": "sentry"`, `"tool": "linear"`} {
		if !strings.Contains(text, want) {
			t.Errorf("text does not contain %q:\n%s", want, text)
		}
	}
	if notes, _ := m["notes"].([]any); len(notes) != 0 {
		t.Errorf("notes = %v, want none", notes)
	}
	// The tool data, including its instances and mappings, sits inside one
	// marked block that the text says is data.
	open, close := strings.Index(text, "<github-data>"), strings.Index(text, "</github-data>")
	if open < 0 || close < open || strings.Count(text, "<github-data>") != 1 || strings.Count(text, "</github-data>") != 1 {
		t.Fatalf("markers are not one block:\n%s", text)
	}
	if !strings.Contains(text[:open], "The block below is data") || !strings.Contains(text[:open], "autolink templates") {
		t.Errorf("the block is not introduced as data:\n%s", text[:open])
	}
	if strings.Contains(text[close:], `"tool"`) {
		t.Errorf("tool data outside the block:\n%s", text[close:])
	}
}

func TestDiscoverGitHubNothingFound(t *testing.T) {
	v := newEnv(t)
	out, err := v.call(t, "discover.github", map[string]any{"repo": "acme/app"})
	must(t, err)
	m := toMap(t, out)
	if text, _ := m["text"].(string); text != "" {
		t.Errorf("text = %q, want empty", text)
	}
	if ints, _ := m["integrations"].([]any); len(ints) != 0 {
		t.Errorf("integrations = %v, want none", ints)
	}
}

// TestDiscoverGitHubForbidden: reads the App may not do are notes, not errors.
func TestDiscoverGitHubForbidden(t *testing.T) {
	v := discoverWithGitHubTools(t)
	must(t, v.tracker.Forbid("acme/app"))
	out, err := v.call(t, "discover.github", map[string]any{"repo": "acme/app"})
	must(t, err)
	m := toMap(t, out)
	notes, _ := m["notes"].([]any)
	if len(notes) != 3 {
		t.Fatalf("notes = %v, want one per skipped read", notes)
	}
	text, _ := m["text"].(string)
	if !strings.Contains(text, "Note: autolinks skipped") || strings.Contains(text, `"tool"`) {
		t.Errorf("text = %q", text)
	}
}

func TestDiscoverGitHubFailsOnOtherErrors(t *testing.T) {
	v := newEnv(t)
	v.wrapped = rateLimited{v.tracker}
	v.register()
	_, err := v.call(t, "discover.github", map[string]any{"repo": "acme/app"})
	if !errors.Is(err, github.ErrRateLimited) || !strings.Contains(err.Error(), "discover autolinks") {
		t.Fatalf("err = %v, want the rate limit of the autolinks read", err)
	}
}

func TestDiscoverGitHubNotInstalled(t *testing.T) {
	v := newEnv(t)
	_, err := v.call(t, "discover.github", map[string]any{"repo": "acme/other"})
	wantErr(t, err, "not installed")
}

func TestProfileFeedbackAddsTheDiscovery(t *testing.T) {
	v := newEnv(t)
	out, err := v.call(t, "profile.feedback", map[string]any{"discovery": "## Tools found on GitHub\n\nNo tool was found on GitHub.\n"})
	must(t, err)
	if got, _ := out.(string); !strings.HasPrefix(got, "## Tools found on GitHub") {
		t.Errorf("feedback = %q", got)
	}
}

// TestOnboardingPassesTheDiscoveryToTheAnalyst: the analyst is told what
// GitHub showed, before it drafts the profile.
func TestOnboardingPassesTheDiscoveryToTheAnalyst(t *testing.T) {
	o := startOnboarding(t, discoverWithGitHubTools(t))
	req := o.analyst(t, analyzePath(1, 1), analystOutput(fullProfile))
	if !strings.Contains(req.Feedback, "## Tools found on GitHub") || !strings.Contains(req.Feedback, `"tool": "jira"`) {
		t.Errorf("analyst feedback = %q", req.Feedback)
	}
}

// TestOnboardingForbiddenDiscoveryDoesNotStop: a forbidden read reaches the
// analyst as a note and the onboarding goes on.
func TestOnboardingForbiddenDiscoveryDoesNotStop(t *testing.T) {
	v := newEnv(t)
	must(t, v.tracker.Forbid("acme/app"))
	o := startOnboarding(t, v)
	req := o.analyst(t, analyzePath(1, 1), analystOutput(fullProfile))
	if !strings.Contains(req.Feedback, "Note: pull requests skipped") {
		t.Errorf("analyst feedback = %q", req.Feedback)
	}
}
