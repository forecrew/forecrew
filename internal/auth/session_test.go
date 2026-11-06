package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// signedIn returns a fixture with a member signed in, and the member.
func signedIn(t *testing.T) (*fixture, domain.User, string) {
	t.Helper()
	f := newFixture(t)
	u := f.member("ana@example.com")
	f.claims("sub-1", "ana@example.com", true)
	f.signIn(f.inst, "")
	token := f.sessionToken()
	if token == "" {
		t.Fatal("not signed in")
	}
	return f, u, token
}

func wantNoSession(t *testing.T, f *fixture, token string) {
	t.Helper()
	if _, err := f.svc.Resolve(t.Context(), token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("resolve = %v, want no session", err)
	}
}

func TestIdleExpiry(t *testing.T) {
	f, _, token := signedIn(t)
	f.clock.Advance(59 * time.Minute)
	if _, err := f.svc.Resolve(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	// Use moved the idle expiry forward.
	f.clock.Advance(59 * time.Minute)
	if _, err := f.svc.Resolve(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(61 * time.Minute)
	wantNoSession(t, f, token)
}

func TestAbsoluteExpiry(t *testing.T) {
	f, _, token := signedIn(t)
	for range 24 {
		f.clock.Advance(59 * time.Minute)
		if _, err := f.svc.Resolve(t.Context(), token); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.Advance(25 * time.Minute) // used all along, past 24 hours
	wantNoSession(t, f, token)
}

func TestIdleNeverPastAbsolute(t *testing.T) {
	f, _, token := signedIn(t)
	f.clock.Advance(23*time.Hour + 30*time.Minute)
	sess, err := f.st.Sessions().GetByTokenHash(t.Context(), mcp.HashToken(token))
	must(t, err)
	must(t, f.st.Sessions().Touch(t.Context(), sess.ID, f.clock.Now().Add(time.Minute)))
	if _, err := f.svc.Resolve(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if sess, _ = f.st.Sessions().GetByTokenHash(t.Context(), mcp.HashToken(token)); sess.IdleExpiresAt.After(sess.ExpiresAt) {
		t.Errorf("session = %+v", sess)
	}
}

func TestRotation(t *testing.T) {
	f, u, first := signedIn(t)
	f.newBrowser()
	f.signIn(f.inst, "")
	second := f.sessionToken()
	if second == "" || second == first {
		t.Fatalf("tokens %q, %q", first, second)
	}
	sessions, err := f.st.Sessions().ListByUser(t.Context(), u.ID)
	must(t, err)
	if len(sessions) != 2 || sessions[0].ID == sessions[1].ID {
		t.Errorf("sessions = %+v", sessions)
	}
}

func TestSwitch(t *testing.T) {
	f, u, token := signedIn(t)
	ctx := t.Context()
	c, err := f.svc.Resolve(ctx, token)
	must(t, err)

	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	if _, err := f.svc.Switch(ctx, c, other.Slug); !errors.Is(err, auth.ErrNotMember) {
		t.Fatalf("non-member = %v", err)
	}
	if _, err := f.svc.Switch(ctx, c, "nowhere"); !errors.Is(err, auth.ErrNotMember) {
		t.Fatalf("unknown = %v", err)
	}
	must(t, f.st.Memberships().Create(ctx, &domain.Membership{AccountID: other.ID, UserID: u.ID, Status: domain.MembershipSuspended, Source: domain.SourceManual}))
	if _, err := f.svc.Switch(ctx, c, other.Slug); !errors.Is(err, auth.ErrNotMember) {
		t.Fatalf("suspended = %v", err)
	}
	must(t, f.st.Memberships().SetStatus(ctx, other.ID, u.ID, domain.MembershipActive))
	acc, err := f.svc.Switch(ctx, c, other.Slug)
	must(t, err)
	if acc.ID != other.ID {
		t.Errorf("account = %+v", acc)
	}
	if c, err = f.svc.Resolve(ctx, token); err != nil || c.Actor.AccountID != other.ID {
		t.Errorf("caller = %+v, %v", c, err)
	}
	tokenCaller := mcp.Caller{Actor: domain.UserActor(other.ID, u.ID, domain.Credential{Kind: domain.CredentialToken, ID: domain.NewID()})}
	if _, err := f.svc.Switch(ctx, tokenCaller, f.org.Slug); err == nil {
		t.Error("a token switched")
	}
}

func TestSignOut(t *testing.T) {
	f, _, token := signedIn(t)
	must(t, f.svc.SignOut(t.Context(), token))
	wantNoSession(t, f, token)
	must(t, f.svc.SignOut(t.Context(), "fcs_unknown"))
	wantNoSession(t, f, "fcs_unknown")
}

func TestSignOutEverywhere(t *testing.T) {
	f, u, first := signedIn(t)
	f.newBrowser()
	f.signIn(f.inst, "")
	second := f.sessionToken()
	must(t, f.svc.SignOutEverywhere(t.Context(), u.ID))
	wantNoSession(t, f, first)
	wantNoSession(t, f, second)
}

func TestSuspensionEndsSessions(t *testing.T) {
	f, u, token := signedIn(t)
	ctx := t.Context()
	must(t, f.st.Memberships().SetStatus(ctx, f.org.ID, u.ID, domain.MembershipSuspended))
	wantNoSession(t, f, token)
	// Ended for good: reactivating the member does not revive it.
	must(t, f.st.Memberships().SetStatus(ctx, f.org.ID, u.ID, domain.MembershipActive))
	wantNoSession(t, f, token)
}

func TestRemovalEndsSessions(t *testing.T) {
	f, u, token := signedIn(t)
	must(t, f.st.Memberships().Remove(t.Context(), f.org.ID, u.ID))
	wantNoSession(t, f, token)
}

func TestDisablingEndsSessions(t *testing.T) {
	f, u, token := signedIn(t)
	ctx := t.Context()
	must(t, f.st.Users().SetDisabled(ctx, u.ID, true))
	wantNoSession(t, f, token)
	must(t, f.st.Users().SetDisabled(ctx, u.ID, false))
	wantNoSession(t, f, token)
}

func TestProfile(t *testing.T) {
	f, u, _ := signedIn(t)
	got, orgs, err := f.svc.Profile(t.Context(), mcp.Caller{Actor: domain.UserActor(f.org.ID, u.ID, domain.Credential{})})
	must(t, err)
	if got.ID != u.ID || len(orgs) != 1 || orgs[0].Slug != f.org.Slug {
		t.Errorf("profile = %+v, %+v", got, orgs)
	}
	if _, _, err := f.svc.Profile(t.Context(), mcp.Caller{Actor: domain.UserActor(f.org.ID, domain.NewID(), domain.Credential{})}); err == nil {
		t.Error("unknown user")
	}
	if _, _, err := f.svc.Profile(t.Context(), mcp.Caller{Actor: domain.SystemActor(f.org.ID, "x")}); err == nil {
		t.Error("not a person")
	}
}
