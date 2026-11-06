package auth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestCLILoginCodes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	u := f.member("ana@acme.example")
	sess := domain.Session{TokenHash: []byte("s"), UserID: u.ID, AccountID: f.org.ID, ExpiresAt: t0.Add(24 * time.Hour), IdleExpiresAt: t0.Add(time.Hour)}
	must(t, f.st.Sessions().Create(ctx, &sess))
	c := mcp.Caller{Actor: domain.UserActor(f.org.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: sess.ID})}
	verifier := strings.Repeat("v", 50)
	in := auth.CLIAuthorizeInput{RedirectURI: "http://[::1]:7000/cb", State: "s", CodeChallenge: challengeOf(verifier)}

	out, err := f.svc.CLIAuthorize(ctx, c, in)
	must(t, err)
	if !strings.HasPrefix(out.Code, auth.PrefixCLICode) || !strings.HasPrefix(out.Redirect, "http://[::1]:7000/cb?") {
		t.Fatalf("out = %+v", out)
	}
	user, org, err := f.svc.CLIExchange(out.Code, verifier)
	if err != nil || user != u.ID || org != f.org.ID {
		t.Fatalf("exchange = %v %v %v", user, org, err)
	}
	if _, _, err := f.svc.CLIExchange(out.Code, verifier); !errors.Is(err, auth.ErrCLICode) {
		t.Errorf("reuse = %v", err)
	}
	out, err = f.svc.CLIAuthorize(ctx, c, in)
	must(t, err)
	f.clock.Advance(6 * time.Minute)
	if _, _, err := f.svc.CLIExchange(out.Code, verifier); !errors.Is(err, auth.ErrCLICode) {
		t.Errorf("expired = %v", err)
	}

	// The organization of the token: named, and the user must be a member.
	in.Organization = f.org.Slug
	if _, err := f.svc.CLIAuthorize(ctx, c, in); err != nil {
		t.Errorf("own organization: %v", err)
	}
	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	in.Organization = other.Slug
	if _, err := f.svc.CLIAuthorize(ctx, c, in); !errors.Is(err, auth.ErrNotMember) {
		t.Errorf("not a member: %v", err)
	}
	in.Organization = ""
	f.st.SetMemberStatus(f.org.ID, u.ID, domain.MembershipSuspended)
	if _, err := f.svc.CLIAuthorize(ctx, c, in); !errors.Is(err, auth.ErrNotMember) {
		t.Errorf("suspended: %v", err)
	}
	// A session without an organization has none to give; a token is no session.
	f.st.SetMemberStatus(f.org.ID, u.ID, domain.MembershipActive)
	noOrg := mcp.Caller{Actor: domain.UserActor(uuid.Nil(), u.ID, domain.Credential{Kind: domain.CredentialSession, ID: sess.ID})}
	if _, err := f.svc.CLIAuthorize(ctx, noOrg, in); !errors.Is(err, auth.ErrNotMember) {
		t.Errorf("no organization: %v", err)
	}
	token := mcp.Caller{Actor: domain.UserActor(f.org.ID, u.ID, domain.Credential{Kind: domain.CredentialToken, ID: uuid.New()})}
	if _, err := f.svc.CLIAuthorize(ctx, token, in); !errors.Is(err, auth.ErrCLIRequest) {
		t.Errorf("token: %v", err)
	}
	// Only loopback addresses, and a bounded number of codes.
	for _, uri := range []string{"http://localhost:1/x", "http://10.0.0.1:1/x", "http://user@127.0.0.1:1/x", "http://127.0.0.1:1/x#f", "ftp://127.0.0.1:1/x", "::"} {
		in.RedirectURI = uri
		if _, err := f.svc.CLIAuthorize(ctx, c, in); !errors.Is(err, auth.ErrCLIRequest) {
			t.Errorf("%s: %v", uri, err)
		}
	}
	in.RedirectURI = "http://127.0.0.1:1/x"
	var full error
	for range 1001 {
		if _, full = f.svc.CLIAuthorize(ctx, c, in); full != nil {
			break
		}
	}
	if full == nil {
		t.Error("the number of codes is not bounded")
	}
}

func TestSessionsAndIdentitiesOfAUser(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	u := f.member("ana@acme.example")
	mk := func(hash string, revoked bool) domain.Session {
		s := domain.Session{TokenHash: []byte(hash), UserID: u.ID, AccountID: f.org.ID, ConnectionID: f.inst.ID, ExpiresAt: t0.Add(24 * time.Hour), IdleExpiresAt: t0.Add(time.Hour)}
		must(t, f.st.Sessions().Create(ctx, &s))
		if revoked {
			must(t, f.st.Sessions().Revoke(ctx, u.ID, s.ID))
		}
		return s
	}
	a, _ := mk("a", false), mk("b", true)
	c := mcp.Caller{Actor: domain.UserActor(f.org.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: a.ID})}
	got, err := f.svc.Sessions(ctx, c)
	if err != nil || len(got) != 1 || !got[0].Current || got[0].Organization != f.org.Slug || got[0].Connection == "" {
		t.Fatalf("sessions = %+v, %v", got, err)
	}
	if err := f.svc.EndSession(ctx, c, uuid.New()); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	service := mcp.Caller{Actor: domain.Actor{Kind: domain.ActorMCP, Name: "x"}}
	if _, err := f.svc.Sessions(ctx, service); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("no user: %v", err)
	}
	if _, err := f.svc.Identities(ctx, service); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("no user: %v", err)
	}
	if err := f.svc.EndSession(ctx, service, a.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("no user: %v", err)
	}
	must(t, f.st.Identities().Create(ctx, &domain.Identity{UserID: u.ID, ConnectionID: f.inst.ID, Subject: "s", Email: "ana@acme.example"}))
	ids, err := f.svc.Identities(ctx, c)
	if err != nil || len(ids) != 1 || ids[0].Kind != string(domain.ConnectionOIDC) {
		t.Fatalf("identities = %+v, %v", ids, err)
	}
}
