//go:build enterprise

package ee

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func isDenied(err error) bool   { return errors.Is(err, authz.ErrDenied) }
func isConflict(err error) bool { return errors.Is(err, mcp.ErrConflict) }
func isInvalid(err error) bool  { return errors.Is(err, mcp.ErrInvalidInput) }
func isNotFound(err error) bool { return errors.Is(err, mcp.ErrNotFound) }

func TestVerifyDomain(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	d, err := e.svc.AddDomain(ctx, e.owner, mcp.DomainInput{Domain: " Acme.Example "})
	must(t, err)
	if d.Domain != "acme.example" || d.RecordName != "_forecrew-challenge.acme.example" || !strings.HasPrefix(d.RecordValue, "forecrew-verification=") {
		t.Fatalf("domain = %+v", d)
	}
	if _, err := e.svc.AddDomain(ctx, e.owner, mcp.DomainInput{Domain: "acme.example"}); !isConflict(err) {
		t.Errorf("twice: %v", err)
	}
	if _, err := e.svc.AddDomain(ctx, e.owner, mcp.DomainInput{Domain: "not a domain"}); !isInvalid(err) {
		t.Errorf("bad name: %v", err)
	}
	if _, err := e.svc.VerifyDomain(ctx, e.owner, mcp.DomainInput{Domain: "acme.example"}); !isConflict(err) {
		t.Errorf("no record: %v", err)
	}
	e.dns.set(d.RecordName, "forecrew-verification=wrong")
	if _, err := e.svc.VerifyDomain(ctx, e.owner, mcp.DomainInput{Domain: "acme.example"}); !isConflict(err) {
		t.Errorf("another value: %v", err)
	}
	e.dns.err = errors.New("timeout")
	if _, err := e.svc.VerifyDomain(ctx, e.owner, mcp.DomainInput{Domain: "acme.example"}); !errors.Is(err, mcp.ErrUnavailable) {
		t.Errorf("DNS down: %v", err)
	}
	e.dns.err = nil
	e.dns.set(d.RecordName, d.RecordValue)
	got, err := e.svc.VerifyDomain(ctx, e.owner, mcp.DomainInput{Domain: d.ID.String()})
	must(t, err)
	if got.VerifiedAt.IsZero() || !e.audited("domain.verified") {
		t.Errorf("verified = %+v", got)
	}
	list, err := e.svc.ListDomains(ctx, e.owner)
	must(t, err)
	if len(list) != 1 || list[0].RecordValue != d.RecordValue {
		t.Errorf("list = %+v", list)
	}
	if _, err := e.svc.VerifyDomain(ctx, e.owner, mcp.DomainInput{Domain: "nope.example"}); !isNotFound(err) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := e.svc.DeleteDomain(ctx, e.owner, mcp.DomainInput{Domain: "acme.example"}); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.ListDomains(ctx, e.owner); len(list) != 0 {
		t.Errorf("after delete = %+v", list)
	}
}

func TestDomainBelongsToOneOrganization(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.verified("acme.example")

	other := domain.Account{Name: "Other"}
	must(t, e.st.Accounts().Create(ctx, &other))
	u := domain.User{Name: "O"}
	must(t, e.st.Users().Create(ctx, &u))
	must(t, e.st.Memberships().Create(ctx, &domain.Membership{AccountID: other.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	testkit.Bind(t, e.st, other.ID, domain.SubjectUser, u.ID, domain.RoleScopeOrganization, domain.RoleOwner, uuid.Nil())
	by := domain.UserActor(other.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})
	d, err := e.svc.AddDomain(ctx, by, mcp.DomainInput{Domain: "acme.example"})
	must(t, err)
	e.dns.set(d.RecordName, d.RecordValue)
	if _, err := e.svc.VerifyDomain(ctx, by, mcp.DomainInput{Domain: "acme.example"}); !isConflict(err) {
		t.Fatalf("verified elsewhere: %v", err)
	}
}

func TestDomainsNeedThePermission(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	by := e.person("viewer@example.com", domain.RoleViewer)
	in := mcp.DomainInput{Domain: "acme.example"}
	calls := map[string]func() error{
		"list":   func() error { _, err := e.svc.ListDomains(ctx, by); return err },
		"add":    func() error { _, err := e.svc.AddDomain(ctx, by, in); return err },
		"verify": func() error { _, err := e.svc.VerifyDomain(ctx, by, in); return err },
		"delete": func() error { _, err := e.svc.DeleteDomain(ctx, by, in); return err },
		"enforce": func() error {
			_, err := e.svc.SetSSOEnforcement(ctx, by, mcp.SetSSOEnforcementInput{Domain: "acme.example"})
			return err
		},
		"ttl": func() error { _, err := e.svc.SetMaxTokenTTL(ctx, by, mcp.SetMaxTokenTTLInput{}); return err },
	}
	for name, call := range calls {
		if err := call(); !isDenied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	admin := e.person("admin@example.com", domain.RoleAdmin)
	if _, err := e.svc.SetMaxTokenTTL(ctx, admin, mcp.SetMaxTokenTTLInput{MaxTokenTTL: "720h"}); !isDenied(err) {
		t.Errorf("only an Owner allows longer: %v", err)
	}
}

func TestLostRecordUnverifiesAndTellsTheOwners(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	project := domain.Project{AccountID: e.org.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main"}
	must(t, e.st.Projects().Create(ctx, &project))
	d := e.verified("acme.example")
	conn := e.connection("okta")
	if _, err := e.svc.SetSSOEnforcement(ctx, e.owner, mcp.SetSSOEnforcementInput{Domain: d.Domain, Enforce: true, Connection: conn.Name}); err != nil {
		t.Fatal(err)
	}

	// A resolver that fails says nothing: enforcement stays.
	e.dns.err = &net.DNSError{Err: "server misbehaving", Name: d.RecordName, IsTemporary: true}
	must(t, e.svc.Recheck(ctx))
	if got, err := e.st.VerifiedDomains().GetVerified(ctx, d.Domain); err != nil || !got.EnforceSSO {
		t.Fatalf("after a DNS failure: %+v, %v", got, err)
	}
	e.dns.err = nil
	must(t, e.svc.Recheck(ctx))
	if got, err := e.st.VerifiedDomains().GetVerified(ctx, d.Domain); err != nil || !got.EnforceSSO {
		t.Fatalf("with the record: %+v, %v", got, err)
	}

	e.dns.set(d.RecordName, "something else")
	must(t, e.svc.Recheck(ctx))
	if _, err := e.st.VerifiedDomains().GetVerified(ctx, d.Domain); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("still verified: %v", err)
	}
	got, err := e.st.VerifiedDomains().Get(ctx, e.org.ID, d.ID)
	must(t, err)
	if got.EnforceSSO || !got.VerifiedAt.IsZero() {
		t.Errorf("domain = %+v", got)
	}
	if !e.audited("domain.verification_lost") {
		t.Error("not audited")
	}
	pending, err := e.st.Gates().ListPendingBySubject(ctx, e.org.ID, "acme/app", 0)
	must(t, err)
	if len(pending) != 1 || pending[0].Kind != domain.GateNeedsYou || !strings.Contains(pending[0].Title, "acme.example") || !strings.Contains(pending[0].Summary, "enforcement of the domain has stopped") {
		t.Fatalf("gates = %+v", pending)
	}
	// Told once: an unverified domain is not looked up again.
	must(t, e.svc.Recheck(ctx))
	if pending, _ = e.st.Gates().ListPendingBySubject(ctx, e.org.ID, "acme/app", 0); len(pending) != 1 {
		t.Errorf("gates = %+v", pending)
	}
}

func TestLostRecordWithoutProjectIsAudited(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	d := e.verified("acme.example")
	delete(e.dns.records, d.RecordName)
	must(t, e.svc.Recheck(ctx))
	if !e.audited("domain.verification_lost") {
		t.Error("not audited")
	}
	// Without a project there is nowhere to open a gate: nobody is told in
	// the app (docs/enterprise.md).
	gates, err := e.st.Gates().List(ctx, e.org.ID, store.GateFilter{})
	must(t, err)
	if len(gates) != 0 {
		t.Errorf("gates = %+v", gates)
	}
}

func TestRunChecksAtOnceAndStops(t *testing.T) {
	e := newEnv(t)
	d := e.verified("acme.example")
	delete(e.dns.records, d.RecordName)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		e.svc.Run(ctx)
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for !e.audited("domain.verification_lost") {
		select {
		case <-deadline:
			t.Fatal("the domain was not checked")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestSSOEnforcement(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	d, err := e.svc.AddDomain(ctx, e.owner, mcp.DomainInput{Domain: "acme.example"})
	must(t, err)
	conn := e.connection("okta")
	in := mcp.SetSSOEnforcementInput{Domain: d.Domain, Enforce: true, Connection: conn.Name}
	if _, err := e.svc.SetSSOEnforcement(ctx, e.owner, in); !isConflict(err) {
		t.Errorf("not verified: %v", err)
	}
	e.dns.set(d.RecordName, d.RecordValue)
	if _, err := e.svc.VerifyDomain(ctx, e.owner, mcp.DomainInput{Domain: d.Domain}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetSSOEnforcement(ctx, e.owner, mcp.SetSSOEnforcementInput{Domain: d.Domain, Enforce: true}); !isInvalid(err) {
		t.Errorf("no connection: %v", err)
	}
	if _, err := e.svc.SetSSOEnforcement(ctx, e.owner, mcp.SetSSOEnforcementInput{Domain: d.Domain, Enforce: true, Connection: "nope"}); !isNotFound(err) {
		t.Errorf("unknown connection: %v", err)
	}
	off := conn
	off.Enabled = false
	must(t, e.st.Connections().Update(ctx, &off))
	if _, err := e.svc.SetSSOEnforcement(ctx, e.owner, in); !isConflict(err) {
		t.Errorf("connection off: %v", err)
	}
	must(t, e.st.Connections().Update(ctx, &conn))

	got, err := e.svc.SetSSOEnforcement(ctx, e.owner, in)
	must(t, err)
	if !got.EnforceSSO || got.ConnectionID != conn.ID {
		t.Fatalf("domain = %+v", got)
	}
	a, err := e.st.Accounts().Get(ctx, e.org.ID)
	must(t, err)
	if time.Duration(a.MaxTokenTTL) != domain.DefaultSSOTokenTTL {
		t.Fatalf("cap = %v", a.MaxTokenTTL)
	}

	// A longer cap an Owner allowed survives turning enforcement on again.
	if _, err := e.svc.SetMaxTokenTTL(ctx, e.owner, mcp.SetMaxTokenTTLInput{MaxTokenTTL: "720h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetSSOEnforcement(ctx, e.owner, mcp.SetSSOEnforcementInput{Domain: d.Domain}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetSSOEnforcement(ctx, e.owner, in); err != nil {
		t.Fatal(err)
	}
	if a, _ = e.st.Accounts().Get(ctx, e.org.ID); time.Duration(a.MaxTokenTTL) != 720*time.Hour {
		t.Errorf("cap = %v", a.MaxTokenTTL)
	}
	if _, err := e.svc.SetMaxTokenTTL(ctx, e.owner, mcp.SetMaxTokenTTLInput{MaxTokenTTL: "soon"}); !isInvalid(err) {
		t.Errorf("bad ttl: %v", err)
	}
	if a, err = e.svc.SetMaxTokenTTL(ctx, e.owner, mcp.SetMaxTokenTTLInput{}); err != nil || a.MaxTokenTTL != 0 {
		t.Errorf("no cap: %+v, %v", a, err)
	}
	if !e.audited("sso.enforcement_changed") || !e.audited("organization.token_ttl_changed") {
		t.Error("not audited")
	}
}
