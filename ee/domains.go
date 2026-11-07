//go:build enterprise

package ee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

const (
	// recordPrefix and recordValuePrefix form the TXT record of a domain.
	recordPrefix      = "_forecrew-challenge."
	recordValuePrefix = "forecrew-verification="
	// dnsTimeout bounds one lookup.
	dnsTimeout = 10 * time.Second
	// RecheckEvery is how often verified domains are looked up again.
	RecheckEvery = 24 * time.Hour
)

// domainRE is the CHECK of verified_domains.domain.
var domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func wrap(what string, err error) error { return fmt.Errorf("%s: %w", what, err) }

func marshal(data map[string]any) ([]byte, error) {
	if data == nil {
		return nil, nil
	}
	return json.Marshal(data)
}

func domainInfo(d domain.VerifiedDomain) mcp.DomainInfo {
	return mcp.DomainInfo{VerifiedDomain: d, RecordName: recordPrefix + d.Domain, RecordValue: recordValuePrefix + d.VerificationToken}
}

// ListDomains implements mcp.EnterpriseTools.
func (s *Service) ListDomains(ctx context.Context, by domain.Actor) ([]mcp.DomainInfo, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return nil, err
	}
	all, err := s.st.VerifiedDomains().List(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.DomainInfo, len(all))
	for i, d := range all {
		out[i] = domainInfo(d)
	}
	return out, nil
}

// AddDomain implements mcp.EnterpriseTools.
func (s *Service) AddDomain(ctx context.Context, by domain.Actor, in mcp.DomainInput) (mcp.DomainInfo, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return mcp.DomainInfo{}, err
	}
	name := strings.ToLower(strings.TrimSpace(in.Domain))
	if !domainRE.MatchString(name) {
		return mcp.DomainInfo{}, wrap(fmt.Sprintf("%q is not a domain name", in.Domain), mcp.ErrInvalidInput)
	}
	token, err := auth.NewToken("")
	if err != nil {
		return mcp.DomainInfo{}, err
	}
	d := domain.VerifiedDomain{AccountID: by.AccountID, Domain: name, VerificationToken: token}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.VerifiedDomains().Create(ctx, &d); err != nil {
			return err
		}
		return audit(ctx, tx, by, "domain.added", "domain", d.ID, map[string]any{"domain": name})
	})
	return domainInfo(d), storeErr(err, "domain "+name+" exists")
}

// domain finds a domain of the organization by name or UUID.
func (s *Service) domain(ctx context.Context, accountID uuid.UUID, ref string) (domain.VerifiedDomain, error) {
	all, err := s.st.VerifiedDomains().List(ctx, accountID)
	if err != nil {
		return domain.VerifiedDomain{}, err
	}
	ref = strings.ToLower(strings.TrimSpace(ref))
	i := slices.IndexFunc(all, func(d domain.VerifiedDomain) bool { return d.Domain == ref || d.ID.String() == ref })
	if i < 0 {
		return domain.VerifiedDomain{}, wrap("domain "+ref, mcp.ErrNotFound)
	}
	return all[i], nil
}

// hasRecord looks the TXT record of d up. Only an answer that the record is
// not there is false: a resolver that fails says nothing about the domain.
func (s *Service) hasRecord(ctx context.Context, d domain.VerifiedDomain) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	records, err := s.dns.LookupTXT(ctx, recordPrefix+d.Domain)
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.Contains(records, recordValuePrefix+d.VerificationToken), nil
}

// VerifyDomain implements mcp.EnterpriseTools.
func (s *Service) VerifyDomain(ctx context.Context, by domain.Actor, in mcp.DomainInput) (mcp.DomainInfo, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return mcp.DomainInfo{}, err
	}
	d, err := s.domain(ctx, by.AccountID, in.Domain)
	if err != nil {
		return mcp.DomainInfo{}, err
	}
	found, err := s.hasRecord(ctx, d)
	if err != nil {
		s.log.WarnContext(ctx, "look up a domain", "domain", d.Domain, "error", err)
		return mcp.DomainInfo{}, wrap("DNS did not answer, try again later", mcp.ErrUnavailable)
	}
	if !found {
		return mcp.DomainInfo{}, wrap("the TXT record of "+d.Domain+" is not there yet", mcp.ErrConflict)
	}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.VerifiedDomains().SetVerified(ctx, by.AccountID, d.ID, true); err != nil {
			return err
		}
		if d, err = tx.VerifiedDomains().Get(ctx, by.AccountID, d.ID); err != nil {
			return err
		}
		return audit(ctx, tx, by, "domain.verified", "domain", d.ID, map[string]any{"domain": d.Domain})
	})
	return domainInfo(d), storeErr(err, "another organization verified "+d.Domain)
}

// DeleteDomain implements mcp.EnterpriseTools.
func (s *Service) DeleteDomain(ctx context.Context, by domain.Actor, in mcp.DomainInput) (mcp.DomainInfo, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return mcp.DomainInfo{}, err
	}
	d, err := s.domain(ctx, by.AccountID, in.Domain)
	if err != nil {
		return mcp.DomainInfo{}, err
	}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.VerifiedDomains().Delete(ctx, by.AccountID, d.ID); err != nil {
			return err
		}
		return audit(ctx, tx, by, "domain.deleted", "domain", d.ID, map[string]any{"domain": d.Domain, "enforced": d.EnforceSSO})
	})
	return domainInfo(d), storeErr(err, "domain "+d.Domain)
}

// SetSSOEnforcement implements mcp.EnterpriseTools.
func (s *Service) SetSSOEnforcement(ctx context.Context, by domain.Actor, in mcp.SetSSOEnforcementInput) (mcp.DomainInfo, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return mcp.DomainInfo{}, err
	}
	d, err := s.domain(ctx, by.AccountID, in.Domain)
	if err != nil {
		return mcp.DomainInfo{}, err
	}
	connection := uuid.Nil()
	if in.Enforce {
		if d.VerifiedAt.IsZero() {
			return mcp.DomainInfo{}, wrap(d.Domain+" is not verified", mcp.ErrConflict)
		}
		if in.Connection == "" {
			return mcp.DomainInfo{}, wrap("connection is required to enforce", mcp.ErrInvalidInput)
		}
		c, err := s.connection(ctx, by.AccountID, in.Connection)
		if err != nil {
			return mcp.DomainInfo{}, err
		}
		if !c.Enabled {
			return mcp.DomainInfo{}, wrap("connection "+c.Name+" is turned off", mcp.ErrConflict)
		}
		connection = c.ID
	}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.VerifiedDomains().SetEnforcement(ctx, by.AccountID, d.ID, in.Enforce, connection); err != nil {
			return err
		}
		if in.Enforce {
			a, err := tx.Accounts().Get(ctx, by.AccountID)
			if err != nil {
				return err
			}
			if a.MaxTokenTTL == 0 {
				if err := tx.Accounts().SetMaxTokenTTL(ctx, by.AccountID, domain.DefaultSSOTokenTTL); err != nil {
					return err
				}
			}
		}
		if d, err = tx.VerifiedDomains().Get(ctx, by.AccountID, d.ID); err != nil {
			return err
		}
		return audit(ctx, tx, by, "sso.enforcement_changed", "domain", d.ID, map[string]any{"domain": d.Domain, "enforce": in.Enforce, "connection_id": connection.String()})
	})
	return domainInfo(d), storeErr(err, "domain "+d.Domain)
}

// SetMaxTokenTTL implements mcp.EnterpriseTools.
func (s *Service) SetMaxTokenTTL(ctx context.Context, by domain.Actor, in mcp.SetMaxTokenTTLInput) (domain.Account, error) {
	if err := s.allow(ctx, by, domain.PermOrgManage); err != nil {
		return domain.Account{}, err
	}
	var ttl time.Duration
	if in.MaxTokenTTL != "" {
		var err error
		if ttl, err = time.ParseDuration(in.MaxTokenTTL); err != nil || ttl <= 0 {
			return domain.Account{}, wrap(fmt.Sprintf("max_token_ttl %q must be a positive duration such as 168h", in.MaxTokenTTL), mcp.ErrInvalidInput)
		}
	}
	var a domain.Account
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Accounts().SetMaxTokenTTL(ctx, by.AccountID, ttl); err != nil {
			return err
		}
		var err error
		if a, err = tx.Accounts().Get(ctx, by.AccountID); err != nil {
			return err
		}
		return audit(ctx, tx, by, "organization.token_ttl_changed", "organization", by.AccountID, map[string]any{"max_token_ttl": ttl.String()})
	})
	return a, err
}

// Run looks every verified domain up now and then once a day until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(RecheckEvery)
	defer t.Stop()
	for {
		if err := s.Recheck(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.WarnContext(ctx, "check verified domains", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Recheck looks every verified domain up. A lost record unverifies the domain,
// which ends its enforcement. A lookup that fails changes nothing: a DNS
// outage must not turn SSO enforcement off.
func (s *Service) Recheck(ctx context.Context) error {
	accounts, err := s.st.Accounts().List(ctx)
	if err != nil {
		return err
	}
	var failed []error
	for _, a := range accounts {
		all, err := s.st.VerifiedDomains().List(ctx, a.ID)
		if err != nil {
			return err
		}
		for _, d := range all {
			if d.VerifiedAt.IsZero() {
				continue
			}
			found, err := s.hasRecord(ctx, d)
			if err != nil {
				s.log.WarnContext(ctx, "look up a domain", "domain", d.Domain, "error", err)
				continue
			}
			if found {
				continue
			}
			if err := s.lose(ctx, d); err != nil {
				failed = append(failed, err)
			}
		}
	}
	return errors.Join(failed...)
}

// lose unverifies d and tells the Owners.
func (s *Service) lose(ctx context.Context, d domain.VerifiedDomain) error {
	by := domain.SystemActor(d.AccountID, domain.ForecrewdName)
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.VerifiedDomains().SetVerified(ctx, d.AccountID, d.ID, false); err != nil {
			return err
		}
		return audit(ctx, tx, by, "domain.verification_lost", "domain", d.ID, map[string]any{"domain": d.Domain, "enforced": d.EnforceSSO})
	})
	if err != nil {
		return fmt.Errorf("unverify %s: %w", d.Domain, err)
	}
	s.log.ErrorContext(ctx, "a verified domain lost its DNS record", "domain", d.Domain, "account_id", d.AccountID)
	return s.tellOwners(ctx, d)
}

// tellOwners opens a needs_you gate, the one thing Forecrew puts in front of
// people: it shows in the inbox of whoever may read the project, Owners
// included. A gate needs a project; an organization without one has only the
// audit log.
func (s *Service) tellOwners(ctx context.Context, d domain.VerifiedDomain) error {
	if s.gates == nil {
		return nil
	}
	projects, err := s.st.Projects().List(ctx, d.AccountID)
	if err != nil || len(projects) == 0 {
		return err
	}
	text := fmt.Sprintf("The DNS TXT record %s%s is gone, so Forecrew no longer treats %s as verified.", recordPrefix, d.Domain, d.Domain)
	if d.EnforceSSO {
		text += " SSO enforcement of the domain has stopped."
	}
	text += " For the Owners of the organization: restore the record and verify the domain again in the organization's settings."
	_, err = s.gates.Open(ctx, gates.OpenRequest{AccountID: d.AccountID, ProjectID: projects[0].ID, Kind: domain.GateNeedsYou,
		Title: "Domain " + d.Domain + " lost its verification", Summary: text, SubjectRepo: projects[0].Repo})
	return err
}
