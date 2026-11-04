package github_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

func sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	secret := []byte("It's a Secret to Everybody")
	body := []byte("Hello, World!")
	// Value from GitHub's webhook validation docs.
	const docs = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	sha1 := "sha1=" + hex.EncodeToString(hmac.New(sha256.New, secret).Sum(nil)[:20])
	tests := []struct {
		name   string
		secret []byte
		body   []byte
		header string
		ok     bool
	}{
		{"documented example", secret, body, docs, true},
		{"valid", secret, body, sign(secret, body), true},
		{"upper-case hex", secret, body, "sha256=" + upper(sign(secret, body)[7:]), true},
		{"wrong secret", []byte("other"), body, sign(secret, body), false},
		{"tampered body", secret, []byte("Hello, World?"), docs, false},
		{"empty secret", nil, body, sign(nil, body), false},
		{"missing header", secret, body, "", false},
		{"wrong algorithm", secret, body, sha1, false},
		{"no prefix", secret, body, docs[7:], false},
		{"not hex", secret, body, "sha256=zz", false},
		{"truncated", secret, body, docs[:20], false},
		{"empty digest", secret, body, "sha256=", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := github.VerifySignature(tt.secret, tt.body, tt.header)
			if tt.ok {
				noErr(t, err)
				return
			}
			wantErr(t, err, github.ErrBadSignature)
		})
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// The payload fixtures are the examples published by octokit/webhooks
// (payload-examples/api.github.com).
const (
	exRepo = "Codertocat/Hello-World"
	exSHA  = "ec26c3e57ca3a959ca5aad62de7213c562f8c821"
)

func TestParseEvent(t *testing.T) {
	tests := []struct {
		kind, file string
		want       github.Event
	}{
		{"issues", "issues_opened.json", github.Event{
			Kind: "issues", Action: "opened", Repo: exRepo, Number: 1, Actor: "Codertocat",
			Body: "It looks like you accidently spelled 'commit' with two 't's.",
		}},
		{"issue_comment", "issue_comment_created.json", github.Event{
			Kind: "issue_comment", Action: "created", Repo: exRepo, Number: 1, Actor: "Codertocat",
			Body: "You are totally right! I'll get this fixed right away.",
		}},
		{"pull_request", "pull_request_closed.json", github.Event{
			Kind: "pull_request", Action: "closed", Repo: exRepo, Number: 2, Actor: "Codertocat", SHA: exSHA,
			Body: "This is a pretty simple change that we need to pull into master.",
		}},
		{"pull_request_review", "pull_request_review_submitted.json", github.Event{
			Kind: "pull_request_review", Action: "submitted", Repo: exRepo, Number: 2, Actor: "Codertocat", SHA: exSHA,
			ReviewState: "commented",
		}},
		{"check_suite", "check_suite_completed.json", github.Event{
			Kind: "check_suite", Action: "completed", Repo: exRepo, Number: 2, Actor: "Codertocat", SHA: exSHA, Conclusion: "success",
		}},
		{"check_suite", "check_suite_no_pr.json", github.Event{
			Kind: "check_suite", Action: "completed", Repo: exRepo, Actor: "Codertocat", SHA: exSHA, Conclusion: "success",
		}},
		{"check_run", "check_run_completed.json", github.Event{
			Kind: "check_run", Action: "completed", Repo: exRepo, Number: 2, Actor: "Codertocat", SHA: exSHA, Conclusion: "success",
		}},
		{"check_run", "check_run_failed.json", github.Event{
			Kind: "check_run", Action: "completed", Repo: exRepo, Number: 2, Actor: "Codertocat", SHA: exSHA, Conclusion: "failure",
		}},
		{"push", "push_main.json", github.Event{
			Kind: "push", Action: "pushed", Repo: exRepo, Actor: "Codertocat", Ref: "refs/heads/main", DefaultBranch: "main",
			Before: "1111111111111111111111111111111111111111", SHA: "2222222222222222222222222222222222222222",
		}},
		{"push", "push_deleted.json", github.Event{
			Kind: "push", Action: "deleted", Repo: exRepo, Actor: "Codertocat", Ref: "refs/heads/old", DefaultBranch: "main",
			Before: "1111111111111111111111111111111111111111", SHA: "0000000000000000000000000000000000000000",
		}},
		{"status", "status.json", github.Event{
			Kind: "status", Repo: exRepo, Actor: "Codertocat", SHA: "6113728f27ae82c7b1a177c8d03f9e96e0adf246", Conclusion: "success",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := github.ParseEvent(tt.kind, fixture(t, "webhooks", tt.file))
			noErr(t, err)
			if got != tt.want {
				t.Fatalf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// patched returns a published example with edit applied to its top-level
// object, for the variants octokit does not publish.
func patched(t *testing.T, file string, edit func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(fixture(t, "webhooks", file), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	noErr(t, err)
	return b
}

func TestParseEventVariants(t *testing.T) {
	merged := patched(t, "pull_request_closed.json", func(m map[string]any) {
		m["pull_request"].(map[string]any)["merged"] = true
	})
	got, err := github.ParseEvent("pull_request", merged)
	noErr(t, err)
	if !got.Merged || got.Action != "closed" || got.Number != 2 {
		t.Fatalf("merged = %+v", got)
	}

	onPR := patched(t, "issue_comment_created.json", func(m map[string]any) {
		m["issue"].(map[string]any)["pull_request"] = map[string]any{"url": "https://api.github.com/repos/Codertocat/Hello-World/pulls/1"}
	})
	got, err = github.ParseEvent("issue_comment", onPR)
	noErr(t, err)
	if !got.IsPR || got.Number != 1 {
		t.Fatalf("comment on PR = %+v", got)
	}
}

func TestParseEventErrors(t *testing.T) {
	_, err := github.ParseEvent("ping", fixture(t, "webhooks", "ping.json"))
	wantErr(t, err, github.ErrUnsupportedEvent)
	_, err = github.ParseEvent("fork", []byte(`{}`))
	wantErr(t, err, github.ErrUnsupportedEvent)
	for _, kind := range []string{"issues", "issue_comment", "pull_request", "pull_request_review", "check_suite", "check_run", "status", "workflow_run", "push"} {
		_, err = github.ParseEvent(kind, []byte(`{not json`))
		wantErr(t, err, github.ErrBadPayload)
	}
}

func TestParseWorkflowRun(t *testing.T) {
	payload := `{"action": "completed", "workflow_run": {"id": 77, "head_sha": "abc", "conclusion": "failure"},
		"repository": {"full_name": "acme/app"}, "sender": {"login": "octocat"}}`
	got, err := github.ParseEvent("workflow_run", []byte(payload))
	noErr(t, err)
	want := github.Event{Kind: "workflow_run", Action: "completed", Repo: "acme/app", Actor: "octocat", SHA: "abc", Conclusion: "failure", RunID: 77}
	if got != want {
		t.Errorf("event = %+v, want %+v", got, want)
	}
}
