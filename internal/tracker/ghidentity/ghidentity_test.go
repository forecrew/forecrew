package ghidentity_test

import (
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/ghidentity"
)

func TestUser(t *testing.T) {
	if got := ghidentity.User(github.User{ID: 5, Login: "ann"}); got != (tracker.User{ID: "5", Name: "ann"}) {
		t.Errorf("user = %+v", got)
	}
	if got := ghidentity.User(github.User{Login: "ghost", Bot: true}); got != (tracker.User{Name: "ghost", Bot: true}) {
		t.Errorf("user without an ID = %+v", got)
	}
}

func TestTicketType(t *testing.T) {
	for _, tc := range []struct {
		typ    string
		labels []string
		want   string
	}{
		{"Bug", nil, "Bug"}, {"", []string{"Bug"}, "bug"}, {"Task", []string{"bug"}, "Task"}, {"", []string{"p1"}, ""},
	} {
		if got := ghidentity.TicketType(tc.typ, tc.labels); got != tc.want {
			t.Errorf("TicketType(%q, %v) = %q, want %q", tc.typ, tc.labels, got, tc.want)
		}
	}
}

func TestIsOwn(t *testing.T) {
	if !ghidentity.IsOwn([]string{"x", "Forecrew:Goal"}) || ghidentity.IsOwn([]string{"forecrew", "bug"}) {
		t.Error("IsOwn")
	}
}

func TestLinkedUser(t *testing.T) {
	ctx := t.Context()
	st := testkit.NewStore(nil)
	a := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(ctx, &a))
	ann := st.AddMember(a.ID, domain.User{Name: "Ann"}, "")
	st.LinkGitHub(ann.ID, 11, "ann")
	for name, tc := range map[string]struct {
		user tracker.User
		want bool
	}{
		"linked":          {tracker.User{ID: "11", Name: "ann"}, true},
		"not linked":      {tracker.User{ID: "12", Name: "bob"}, false},
		"no ID":           {tracker.User{Name: "ann"}, false},
		"not a GitHub ID": {tracker.User{ID: "abc"}, false},
		"negative":        {tracker.User{ID: "-1"}, false},
	} {
		id, ok, err := ghidentity.LinkedUser(ctx, st, a.ID, tc.user)
		if err != nil || ok != tc.want || (ok && id != ann.ID) {
			t.Errorf("%s: %s, %v, %v", name, id, ok, err)
		}
	}
	// Two members with one GitHub account is no answer.
	bob := st.AddMember(a.ID, domain.User{Name: "Bob"}, "")
	st.LinkGitHub(bob.ID, 11, "ann")
	if _, ok, err := ghidentity.LinkedUser(ctx, st, a.ID, tracker.User{ID: "11"}); err != nil || ok {
		t.Errorf("ambiguous: %v, %v", ok, err)
	}
	// Another organization's members are not found.
	if _, ok, err := ghidentity.LinkedUser(ctx, st, domain.NewID(), tracker.User{ID: "11"}); err != nil || ok {
		t.Errorf("another organization: %v, %v", ok, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
