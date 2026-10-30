package testkit

import (
	"encoding/json"

	"github.com/forecrew/forecrew/internal/github"
)

// Event is a webhook delivery the fake GitHub would send: the value of the
// X-GitHub-Event header and the JSON body.
type Event struct {
	Kind    string
	Payload []byte
}

// OnEvent registers fn to receive the events of the tracker: a PR merged or
// closed, a comment added with AddComment, the checks of a commit set with
// SetCheckStatus or requested with RequestChecks. fn is called after the
// change, outside the tracker's lock, so it may call the tracker.
func (t *Tracker) OnEvent(fn func(Event)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sinks = append(t.sinks, fn)
}

// fire hands ev to the registered functions; the zero Event is not sent.
func (t *Tracker) fire(ev Event) {
	if ev.Kind == "" {
		return
	}
	t.mu.Lock()
	sinks := append([]func(Event){}, t.sinks...)
	t.mu.Unlock()
	for _, fn := range sinks {
		fn(ev)
	}
}

// openPRsAt returns the numbers of the open PRs whose head is sha.
func (r *fakeRepo) openPRsAt(sha string) []int {
	var out []int
	for n, pr := range r.prs {
		if pr.State == "open" && pr.HeadSHA == sha {
			out = append(out, n)
		}
	}
	return out
}

type object = map[string]any

func newEvent(kind string, body object) Event {
	b, err := json.Marshal(body)
	if err != nil {
		panic(err) // the bodies are built from strings and numbers
	}
	return Event{Kind: kind, Payload: b}
}

func repository(repo string) object { return object{"full_name": repo} }

func sender(login string) object { return object{"login": login} }

func prClosedEvent(repo string, pr github.PullRequest) Event {
	return newEvent("pull_request", object{
		"action": "closed", "number": pr.Number, "repository": repository(repo), "sender": sender("octocat"),
		"pull_request": object{"number": pr.Number, "merged": pr.Merged, "head": object{"sha": pr.HeadSHA}, "body": pr.Body},
	})
}

func checkSuiteEvent(repo, action, sha, conclusion string, prs []int) Event {
	numbers := make([]object, 0, len(prs))
	for _, n := range prs {
		numbers = append(numbers, object{"number": n})
	}
	suite := object{"head_sha": sha, "pull_requests": numbers}
	if conclusion != "" {
		suite["conclusion"] = conclusion
	}
	return newEvent("check_suite", object{
		"action": action, "repository": repository(repo), "sender": sender("github-actions[bot]"), "check_suite": suite,
	})
}

func commentEvent(repo string, number int, isPR bool, c github.Comment) Event {
	issue := object{"number": number}
	if isPR {
		issue["pull_request"] = object{}
	}
	return newEvent("issue_comment", object{
		"action": "created", "repository": repository(repo), "sender": sender(c.Author),
		"issue": issue, "comment": object{"id": c.ID, "body": c.Body, "user": sender(c.Author)},
	})
}
