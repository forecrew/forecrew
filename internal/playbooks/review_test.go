package playbooks_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

// developerOutput is a developer output.
func developerOutput(summary string) map[string]any {
	return map[string]any{
		"summary": summary, "files": []any{}, "tests_added": []any{}, "commands_run": []any{}, "escalation": nil,
		"assumptions": []any{map[string]any{"text": "Amounts are in cents", "confidence": "medium", "impact": "high"}},
	}
}

func TestGoalPRText(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "Charge cards")[0]
	out, err := p.call(t, "goal.pr_text", map[string]any{"task_id": task.ID.String(), "result": developerOutput("Adds the charge call.")})
	must(t, err)
	m := toMap(t, out)
	if m["title"] != "TASK-1: Charge cards" || m["ready"] != true {
		t.Errorf("title = %v", m["title"])
	}
	body := m["body"].(string)
	for _, want := range []string{
		"Adds the charge call.", "## Task TASK-1: Charge cards", "**AC-1** A payment succeeds.",
		"Amounts are in cents (confidence medium, impact high)", "Closes #" + itoa(p.taskNow(t, task.ID).IssueNumber),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
}

func TestGoalPRTextErrors(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	stuck := developerOutput("x")
	stuck["escalation"] = "The payment sandbox is missing."
	with := map[string]any{"task_id": task.ID.String(), "result": stuck}
	out, err := p.callKey(t, "goal.pr_text", "k", with)
	must(t, err)
	if m := toMap(t, out); m["ready"] != false || m["title"] != "" || m["body"] != "" {
		t.Errorf("output = %v", m)
	}
	// The reason is commented once per call.
	_, err = p.callKey(t, "goal.pr_text", "k", with)
	must(t, err)
	comments, _ := p.tracker.ListComments(t.Context(), "acme/app", p.taskNow(t, task.ID).IssueNumber, t0.Add(-1))
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "needs a human: The payment sandbox is missing.") {
		t.Errorf("comments = %+v", comments)
	}
	p.wrapped = failing{Tracker: p.tracker, errs: map[string]error{"CreateComment": errors.New("boom")}}
	p.register()
	_, err = p.call(t, "goal.pr_text", with)
	wantErr(t, err, "boom")
	_, err = p.call(t, "goal.pr_text", map[string]any{"task_id": "00000000-0000-0000-0000-000000000000", "result": developerOutput("x")})
	wantErr(t, err, "get task")
	_, err = p.call(t, "goal.pr_text", map[string]any{"task_id": task.ID.String(), "x": 1})
	wantErr(t, err, "unknown field")
	_, err = p.call(t, "goal.pr_text", map[string]any{"task_id": task.ID.String()})
	wantErr(t, err, "with: result: is empty")
}

// recordReviews records the reviews posted through it.
type recordReviews struct {
	github.Tracker
	mu      sync.Mutex
	reviews []github.ReviewInput
	err     error
}

func (r *recordReviews) CreateReview(ctx context.Context, repo string, number int, in github.ReviewInput) (github.Review, error) {
	r.mu.Lock()
	r.reviews = append(r.reviews, in)
	r.mu.Unlock()
	if r.err != nil {
		return github.Review{}, r.err
	}
	return r.Tracker.CreateReview(ctx, repo, number, in)
}

// reviewOutput is a reviewer output with a comment of each kind.
func reviewOutput(verdict string) map[string]any {
	return map[string]any{
		"verdict": verdict, "summary": "The charge is not retried.",
		"comments": []any{
			map[string]any{"path": "pay.go", "line": 12, "severity": "major", "body": "Retry on timeout."},
			map[string]any{"path": "pay.go", "line": nil, "severity": "minor", "body": "Split this file."},
			map[string]any{"path": nil, "line": nil, "severity": "nit", "body": "Typo in the PR text."},
		},
		"criteria_check": []any{
			map[string]any{"criterion": "AC-1", "status": "not_met", "evidence": "No retry test."},
		},
		"assumptions": []any{},
	}
}

func (p *planned) openPR(t *testing.T) github.PullRequest {
	t.Helper()
	pr, err := p.tracker.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "t", Body: "b", Head: "agent/TASK-1", Base: "main"})
	must(t, err)
	return pr
}

func TestGoalReviewPost(t *testing.T) {
	for _, verdict := range []string{"request_changes", "approve"} {
		t.Run(verdict, func(t *testing.T) {
			p := newPlanned(t)
			rec := &recordReviews{Tracker: p.tracker}
			p.wrapped = rec
			p.register()
			pr := p.openPR(t)
			with := map[string]any{"repo": "acme/app", "number": pr.Number, "review": reviewOutput(verdict)}
			out, err := p.callKey(t, "goal.review_post", "k1", with)
			must(t, err)
			if toMap(t, out)["posted"] != true || len(rec.reviews) != 1 {
				t.Fatalf("output = %v, %d reviews", out, len(rec.reviews))
			}
			checkReview(t, rec.reviews[0], verdict)

			// The same call posts nothing more; another call posts again.
			out, err = p.callKey(t, "goal.review_post", "k1", with)
			must(t, err)
			if toMap(t, out)["posted"] != false || len(rec.reviews) != 1 {
				t.Errorf("repeated call: %v, %d reviews", out, len(rec.reviews))
			}
			if _, err = p.callKey(t, "goal.review_post", "k2", with); err != nil || len(rec.reviews) != 2 {
				t.Errorf("second review: %v, %d reviews", err, len(rec.reviews))
			}
		})
	}
}

// checkReview checks the review posted for reviewOutput(verdict).
func checkReview(t *testing.T, in github.ReviewInput, verdict string) {
	t.Helper()
	if in.Event != github.ReviewComment {
		t.Errorf("event = %s: the reviewer must only comment", in.Event)
	}
	want := []string{"<!-- forecrew:review:k1 -->", "**Verdict: " + strings.ReplaceAll(verdict, "_", " ") + "**", "The charge is not retried.",
		"- **AC-1** not met: No retry test.", "- `pay.go`: **minor**: Split this file.", "- **nit**: Typo in the PR text."}
	for _, w := range want {
		if !strings.Contains(in.Body, w) {
			t.Errorf("body lacks %q:\n%s", w, in.Body)
		}
	}
	if strings.Contains(in.Body, "Retry on timeout") || len(in.Comments) != 1 ||
		in.Comments[0] != (github.InlineComment{Path: "pay.go", Line: 12, Body: "**major**: Retry on timeout."}) {
		t.Errorf("inline comments = %+v", in.Comments)
	}
}

func TestGoalReviewPostErrors(t *testing.T) {
	p := newPlanned(t)
	pr := p.openPR(t)
	with := map[string]any{"repo": "acme/app", "number": pr.Number, "review": reviewOutput("approve")}
	_, err := p.call(t, "goal.review_post", map[string]any{"repo": "acme/app", "number": 999, "review": reviewOutput("approve")})
	wantErr(t, err, "list the reviews of acme/app#999")
	p.wrapped = &recordReviews{Tracker: p.tracker, err: errors.New("boom")}
	p.register()
	_, err = p.call(t, "goal.review_post", with)
	wantErr(t, err, "review acme/app#")
	with["x"] = 1
	_, err = p.call(t, "goal.review_post", with)
	wantErr(t, err, "unknown field")
	_, err = p.call(t, "goal.review_post", map[string]any{"repo": "acme/app", "number": pr.Number})
	wantErr(t, err, "with: review: is empty")
}
