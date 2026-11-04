package webhook

import (
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
)

// issue is issue 1 of the repo, the subject of the gates in these tests.
func (e *env) issue() {
	e.t.Helper()
	i, err := e.tracker.CreateIssue(e.t.Context(), repo, github.IssueInput{Title: "goal"})
	must(e.t, err)
	if i.Number != 1 {
		e.t.Fatalf("issue number = %d", i.Number)
	}
}

func (e *env) replies() []string {
	e.t.Helper()
	cs, err := e.tracker.ListComments(e.t.Context(), repo, 1, t0.Add(-1))
	must(e.t, err)
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Body
	}
	return out
}

func TestCommandApprove(t *testing.T) {
	e := newEnv(t)
	e.issue()
	g := e.openGate(domain.GateSpecApproval, 1)
	e.send("d1", "issue_comment", fixture(t, "issue_comment_approve.json"))
	got := e.gate(g)
	if got.Status != domain.GateApproved || got.ResolvedBy != "github:octocat" {
		t.Fatalf("gate = %s by %s", got.Status, got.ResolvedBy)
	}
	if len(e.signals(domain.GateKey(g.ID))) != 1 {
		t.Fatal("no gate.resolved signal")
	}
	if r := e.replies(); len(r) != 1 || r[0] != g.Ref()+": approved." {
		t.Fatalf("replies = %q", r)
	}
}

func TestCommandChanges(t *testing.T) {
	e := newEnv(t)
	e.issue()
	g := e.openGate(domain.GateSpecApproval, 1)
	e.send("d1", "issue_comment", fixture(t, "issue_comment_changes.json"))
	got := e.gate(g)
	if got.Status != domain.GateChangesRequested || got.Feedback != "rename the flag\nand add a test" || got.ResolvedBy != "github:OctoCat" {
		t.Fatalf("gate = %+v", got)
	}
	if r := e.replies(); len(r) != 1 || !strings.Contains(r[0], "changes requested") {
		t.Fatalf("replies = %q", r)
	}
}

func TestCommandChangesNeedsFeedbackAndKind(t *testing.T) {
	e := newEnv(t)
	e.issue()
	g := e.openGate(domain.GateSpecApproval, 1)
	e.send("d1", "issue_comment", fixture(t, "issue_comment_changes_empty.json"))
	if e.gate(g).Status != domain.GatePending {
		t.Fatal("gate resolved without feedback")
	}
	must(t, e.resolveDirect(g))
	n := e.openGate(domain.GateNeedsYou, 1)
	e.send("d2", "issue_comment", fixture(t, "issue_comment_changes.json"))
	if e.gate(n).Status != domain.GatePending {
		t.Fatal("needs_you gate sent back for changes")
	}
	r := e.replies()
	if len(r) != 2 || !strings.Contains(r[0], "Add what should change") || !strings.Contains(r[1], "cannot be sent back") {
		t.Fatalf("replies = %q", r)
	}
}

func TestCommandStatus(t *testing.T) {
	e := newEnv(t)
	e.issue()
	e.send("d1", "issue_comment", fixture(t, "issue_comment_status.json"))
	g := e.openGate(domain.GateMergeApproval, 1)
	e.send("d2", "issue_comment", fixture(t, "issue_comment_status.json"))
	r := e.replies()
	if len(r) != 2 || r[0] != "Nothing is waiting for a decision here." || !strings.Contains(r[1], g.Ref()+" (merge approval)") {
		t.Fatalf("replies = %q", r)
	}
	if e.gate(g).Status != domain.GatePending {
		t.Fatal("status resolved the gate")
	}
}

func TestCommandWithoutGate(t *testing.T) {
	e := newEnv(t)
	e.issue()
	e.send("d1", "issue_comment", fixture(t, "issue_comment_approve.json"))
	if r := e.replies(); len(r) != 1 || r[0] != "Nothing is waiting for a decision here." {
		t.Fatalf("replies = %q", r)
	}
}

func TestCommandFromStrangerIsAuditedOnly(t *testing.T) {
	e := newEnv(t)
	e.issue()
	g := e.openGate(domain.GateSpecApproval, 1)
	e.send("d1", "issue_comment", fixture(t, "issue_comment_stranger.json"))
	if e.gate(g).Status != domain.GatePending {
		t.Fatal("stranger resolved a gate")
	}
	if r := e.replies(); len(r) != 0 {
		t.Fatalf("replied to a stranger: %q", r)
	}
	log, err := e.st.Audit().List(t.Context(), e.account, store.AuditFilter{})
	must(t, err)
	var found bool
	for _, a := range log {
		if a.Action == "command.ignored" {
			found = true
			if a.Actor != "github:mallory" || a.ProjectID != e.project.ID || !strings.Contains(string(a.Data), `"command":"approve"`) {
				t.Fatalf("audit = %+v (%s)", a, a.Data)
			}
		}
	}
	if !found {
		t.Fatalf("no audit entry: %+v", log)
	}
}

func TestNilAllowlistAllowsNobody(t *testing.T) {
	e := newEnv(t)
	e.issue()
	g := e.openGate(domain.GateSpecApproval, 1)
	e.eng.Allowed = nil
	e.send("d1", "issue_comment", fixture(t, "issue_comment_approve.json"))
	if e.gate(g).Status != domain.GatePending || len(e.replies()) != 0 {
		t.Fatal("a command was run without an allowlist")
	}
}

func TestCommentsWithoutCommands(t *testing.T) {
	e := newEnv(t)
	e.issue()
	g := e.openGate(domain.GateSpecApproval, 1)
	e.send("d1", "issue_comment", fixture(t, "issue_comment_plain.json"))
	e.send("d2", "issue_comment", fixture(t, "issue_comment_edited.json"))
	if e.gate(g).Status != domain.GatePending || len(e.replies()) != 0 {
		t.Fatal("a comment without a new command did something")
	}
}

func TestCommandReplyError(t *testing.T) {
	e := newEnv(t)
	// Issue 1 does not exist in the tracker, so the reply fails after the decision.
	g := e.openGate(domain.GateSpecApproval, 1)
	err := e.eng.handle(t.Context(), github.Event{Kind: "issue_comment", Action: "created", Repo: repo, Number: 1, Actor: "octocat", Body: "@forecrew approve"})
	if err == nil || e.gate(g).Status != domain.GateApproved {
		t.Fatalf("err = %v, gate = %s", err, e.gate(g).Status)
	}
}

func TestParseCommand(t *testing.T) {
	for _, tt := range []struct {
		body, cmd, assumption, feedback string
		ok                              bool
	}{
		{"@forecrew approve", "approve", "", "", true},
		{"ok\n@FORECREW   Approve please", "approve", "", "please", true},
		{"@forecrew changes  fix it \n", "changes", "", "fix it", true},
		{"@forecrew status", "status", "", "", true},
		{"@forecrew approve a-3 fine", "approve", "A-3", "fine", true},
		{"@forecrew changes A-12 check the limit", "changes", "A-12", "check the limit", true},
		{"@forecrew approve\nA-3", "approve", "", "A-3", true},
		{"@forecrew approve A-3x", "approve", "", "A-3x", true},
		{"@forecrew approved", "", "", "", false},
		{"@forecrewapprove", "", "", "", false},
		{"forecrew approve", "", "", "", false},
		{"", "", "", "", false},
	} {
		cmd, assumption, fb, ok := parseCommand(tt.body)
		if cmd != tt.cmd || assumption != tt.assumption || fb != tt.feedback || ok != tt.ok {
			t.Errorf("parseCommand(%q) = %q, %q, %q, %v", tt.body, cmd, assumption, fb, ok)
		}
	}
}

// openReview opens the review gate of assumption A-n on the issue.
func (e *env) openReview(n int, title string, task uuid.UUID) domain.Gate {
	e.t.Helper()
	g, err := e.gates.Open(e.t.Context(), gates.OpenRequest{
		AccountID: e.account, ProjectID: e.project.ID, Kind: domain.GateAssumptionReview, Title: "Review A-" + strconv.Itoa(n) + ": " + title,
		SubjectRepo: "Acme/App", SubjectNumber: 1, TaskID: task,
	})
	must(e.t, err)
	return g
}

// The review of an assumption sits on the same issue as the spec gate, and
// is older: "@forecrew approve" meant for the spec must not accept it.
func TestCommandsLeaveAssumptionReviewsAlone(t *testing.T) {
	e := newEnv(t)
	e.issue()
	review := e.openReview(3, "Review A-99: hostile", uuid.Nil())
	spec := e.openGate(domain.GateSpecApproval, 1)
	comment := func(body string) {
		must(t, e.eng.handle(t.Context(), github.Event{Kind: "issue_comment", Action: "created", Repo: repo, Number: 1, Actor: "octocat", Body: body}))
	}

	comment("@forecrew approve")
	if e.gate(spec).Status != domain.GateApproved || e.gate(review).Status != domain.GatePending {
		t.Fatalf("spec = %s, review = %s", e.gate(spec).Status, e.gate(review).Status)
	}
	// Only the review is left: the command says how to decide it.
	comment("@forecrew approve")
	if e.gate(review).Status != domain.GatePending {
		t.Fatalf("review = %s", e.gate(review).Status)
	}
	if r := e.replies(); !strings.Contains(r[len(r)-1], "@forecrew approve A-3") || !strings.Contains(r[len(r)-1], review.Ref()) {
		t.Fatalf("replies = %q", r)
	}
	// Another assumption's name does not reach it; its own does.
	comment("@forecrew approve A-4")
	if e.gate(review).Status != domain.GatePending {
		t.Fatalf("review = %s", e.gate(review).Status)
	}
	if r := e.replies(); r[len(r)-1] != "No review of A-4 is waiting here." {
		t.Fatalf("replies = %q", r)
	}
	comment("@forecrew changes A-3 check the rounding")
	if got := e.gate(review); got.Status != domain.GateChangesRequested || got.Feedback != "check the rounding" {
		t.Fatalf("review = %+v", got)
	}
}
