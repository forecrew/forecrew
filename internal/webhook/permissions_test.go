package webhook

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// octocatID is the GitHub user ID of the linked member in these tests.
const octocatID = 583231

// linked adds the member behind octocat with a role on the project; an empty
// role leaves it without one, and someone else holds a role instead, so the
// organization is enforced.
func (e *env) linked(role string) uuid.UUID {
	e.t.Helper()
	u := e.st.AddMember(e.account, domain.User{Name: "Octo"}, "")
	e.st.LinkGitHub(u.ID, octocatID, "octocat")
	if role != "" {
		testkit.Bind(e.t, e.st, e.account, domain.SubjectUser, u.ID, domain.RoleScopeProject, role, e.project.ID)
		return u.ID
	}
	e.enforced()
	return u.ID
}

// enforced gives the organization its first role binding.
func (e *env) enforced() {
	e.t.Helper()
	other := e.st.AddMember(e.account, domain.User{Name: "Ana"}, "")
	testkit.Bind(e.t, e.st, e.account, domain.SubjectUser, other.ID, domain.RoleScopeOrganization, domain.RoleOwner, uuid.Nil())
}

func (e *env) comment(body string) {
	e.t.Helper()
	must(e.t, e.eng.handle(e.t.Context(), github.Event{Kind: "issue_comment", Action: "created", Repo: repo, Number: 1, Actor: "octocat", ActorID: octocatID, Body: body}))
}

func (e *env) auditActions() []string {
	e.t.Helper()
	entries, err := e.st.Audit().List(e.t.Context(), e.account, store.AuditFilter{})
	must(e.t, err)
	out := make([]string, len(entries))
	for i, a := range entries {
		out[i] = a.Action
	}
	return out
}

// TestCommandAccessCoversEveryCommand fails when a command has no permission:
// the commands are the entries, and every entry is well formed.
func TestCommandAccessCoversEveryCommand(t *testing.T) {
	access := CommandAccess()
	for _, cmd := range slices.Sorted(maps.Keys(access)) {
		if err := mcp.CheckAccess(access[cmd]); err != nil {
			t.Errorf("%s: %v", cmd, err)
		}
		if a := access[cmd]; !a.Gate && a.Permission == "" {
			t.Errorf("%s needs nothing", cmd)
		}
		if got, _, _, ok := parseCommand("@forecrew " + cmd); !ok || got != cmd {
			t.Errorf("%s does not parse: %q", cmd, got)
		}
	}
	if _, _, _, ok := parseCommand("@forecrew merge"); ok {
		t.Error("a command without an entry parses")
	}
}

func TestLinkedUserResolvesWithThePermission(t *testing.T) {
	e := newEnv(t)
	e.issue()
	id := e.linked(domain.RoleMaintainer)
	g := e.openGate(domain.GateMergeApproval, 1)
	e.comment("@forecrew approve")
	if got := e.gate(g); got.Status != domain.GateApproved || got.ResolvedBy != "user:"+id.String() {
		t.Fatalf("gate = %+v", got)
	}
}

// TestCommandRefusedWithoutThePermission is a Contributor who may approve
// specs but not merges.
func TestCommandRefusedWithoutThePermission(t *testing.T) {
	e := newEnv(t)
	e.issue()
	e.linked(domain.RoleContributor)
	merge := e.openGate(domain.GateMergeApproval, 1)
	e.comment("@forecrew approve")
	if got := e.gate(merge); got.Status != domain.GatePending {
		t.Fatalf("gate = %+v", got)
	}
	r := e.replies()
	if len(r) != 1 || !strings.Contains(r[0], "gate.approve.merge") || strings.Contains(r[0], e.project.ID.String()) {
		t.Fatalf("replies = %q", r)
	}
	if got := e.auditActions(); !slices.Contains(got, "access.denied") {
		t.Errorf("audit = %v", got)
	}
	e.comment("@forecrew changes do it again")
	if got := e.gate(merge); got.Status != domain.GatePending {
		t.Fatalf("gate = %+v", got)
	}
}

func TestContributorApprovesASpecFromGitHub(t *testing.T) {
	e := newEnv(t)
	e.issue()
	e.linked(domain.RoleContributor)
	g := e.openGate(domain.GateSpecApproval, 1)
	e.comment("@forecrew approve")
	if got := e.gate(g); got.Status != domain.GateApproved {
		t.Fatalf("gate = %+v", got)
	}
}

func TestStatusNeedsGateRead(t *testing.T) {
	e := newEnv(t)
	e.issue()
	e.linked("")
	g := e.openGate(domain.GateSpecApproval, 1)
	e.comment("@forecrew status")
	r := e.replies()
	if len(r) != 1 || !strings.Contains(r[0], "gate.read") || strings.Contains(r[0], g.Ref()) {
		t.Fatalf("replies = %q", r)
	}
}

func TestSuspendedMemberIsRefused(t *testing.T) {
	e := newEnv(t)
	e.issue()
	id := e.linked(domain.RoleMaintainer)
	e.st.SetMemberStatus(e.account, id, domain.MembershipSuspended)
	g := e.openGate(domain.GateSpecApproval, 1)
	e.comment("@forecrew approve")
	if got := e.gate(g); got.Status != domain.GatePending {
		t.Fatalf("gate = %+v", got)
	}
}

// TestUnlinkedAuthorIsRefused: once somebody holds a role only permissions
// decide, and the allowlist of the config is not a way around them.
func TestUnlinkedAuthorIsRefused(t *testing.T) {
	e := newEnv(t)
	e.issue()
	e.enforced()
	g := e.openGate(domain.GateSpecApproval, 1)
	e.comment("@forecrew approve")
	if got := e.gate(g); got.Status != domain.GatePending {
		t.Fatalf("gate = %+v", got)
	}
	if r := e.replies(); len(r) != 1 || !strings.Contains(r[0], "linked GitHub account") {
		t.Fatalf("replies = %q", r)
	}
	if got := e.auditActions(); !slices.Contains(got, "command.ignored") {
		t.Errorf("audit = %v", got)
	}
}

// TestRenamedLoginIsNotAUser: the login of a stored identity may have been
// taken by somebody else, so an ID that does not match wins over it.
func TestRenamedLoginIsNotAUser(t *testing.T) {
	e := newEnv(t)
	e.issue()
	e.linked(domain.RoleMaintainer)
	g := e.openGate(domain.GateSpecApproval, 1)
	must(t, e.eng.handle(t.Context(), github.Event{Kind: "issue_comment", Action: "created", Repo: repo, Number: 1, Actor: "octocat", ActorID: 7, Body: "@forecrew approve"}))
	if got := e.gate(g); got.Status != domain.GatePending {
		t.Fatalf("gate = %+v", got)
	}
}

func TestAmbiguousLoginIsNotAUser(t *testing.T) {
	e := newEnv(t)
	e.issue()
	e.linked(domain.RoleMaintainer)
	twin := e.st.AddMember(e.account, domain.User{Name: "Twin"}, "")
	e.st.LinkGitHub(twin.ID, octocatID, "octocat")
	g := e.openGate(domain.GateSpecApproval, 1)
	e.comment("@forecrew approve")
	if got := e.gate(g); got.Status != domain.GatePending {
		t.Fatalf("gate = %+v", got)
	}
}

// accessFailing cannot read who wrote a comment.
type accessFailing struct {
	store.Store
	directory store.DirectoryRepo
	bindings  store.RoleBindingRepo
}

func (s accessFailing) Directory() store.DirectoryRepo      { return s.directory }
func (s accessFailing) RoleBindings() store.RoleBindingRepo { return s.bindings }

type brokenDirectory struct{ store.DirectoryRepo }

func (brokenDirectory) GitHubMembers(_ context.Context, _ uuid.UUID, _ int64, _ string) ([]uuid.UUID, error) {
	return nil, errBroken
}

type brokenBindings struct{ store.RoleBindingRepo }

func (brokenBindings) List(context.Context, uuid.UUID, store.RoleBindingFilter) ([]domain.RoleBinding, error) {
	return nil, errBroken
}

var errBroken = errors.New("broken")

func TestCommanderStoreErrors(t *testing.T) {
	e := newEnv(t)
	e.issue()
	ev := github.Event{Kind: "issue_comment", Action: "created", Repo: repo, Number: 1, Actor: "octocat", Body: "@forecrew approve"}
	e.eng.Store = accessFailing{e.st, brokenDirectory{e.st.Directory()}, e.st.RoleBindings()}
	if err := e.eng.handle(t.Context(), ev); !errors.Is(err, errBroken) {
		t.Errorf("directory: %v", err)
	}
	e.eng.Store = accessFailing{e.st, e.st.Directory(), brokenBindings{e.st.RoleBindings()}}
	if err := e.eng.handle(t.Context(), ev); !errors.Is(err, errBroken) {
		t.Errorf("bindings: %v", err)
	}
}
