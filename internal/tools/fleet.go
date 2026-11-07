package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// List sizes of list_machines.
const (
	DefaultMachineLimit = 100
	MaxMachineLimit     = 500
)

// ListMachines implements mcp.ToolService.
func (s *Service) ListMachines(ctx context.Context, by domain.Actor, in mcp.ListMachinesInput) ([]mcp.MachineInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	visible, err := s.scope(ctx, by, domain.PermJobRead, uuid.Nil())
	if err != nil {
		return nil, err
	}
	state := domain.MachineState(in.State)
	if in.Limit < 0 || (state != "" && !slices.Contains(domain.MachineStates(), state)) {
		return nil, fmt.Errorf("state %q or limit %d: %w", in.State, in.Limit, mcp.ErrInvalidInput)
	}
	if in.Limit == 0 {
		in.Limit = DefaultMachineLimit
	}
	machines, err := s.cfg.Store.Machines().List(ctx, store.MachineFilter{
		AccountID: by.AccountID, State: state, Active: state == "", Limit: min(in.Limit, MaxMachineLimit),
	})
	if err != nil {
		return nil, err
	}
	shapes, providers, err := s.fleetNames(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.MachineInfo, len(machines))
	for i, m := range machines {
		shape := shapes[m.ShapeID]
		if shape.Ownership() == domain.OwnershipHosted {
			m.ExternalID = "" // the instance's own business
		}
		out[i] = mcp.MachineInfo{Machine: m, Name: fleet.MachineName(shape, m), Shape: shape.Name, Provider: providers[m.ProviderID].Name}
		wrapMachine(&out[i], in.WrapUntrusted)
		if m.JobID == uuid.Nil() {
			continue
		}
		j, err := s.cfg.Store.Jobs().Get(ctx, by.AccountID, m.JobID)
		if err != nil {
			return nil, err
		}
		if visible.Has(j.ProjectID) {
			out[i].JobRef = j.Ref()
		} else {
			out[i].JobID = uuid.Nil() // a job of a project the caller cannot read
		}
	}
	return out, nil
}

// fleetNames returns what the organization can see of shapes and providers,
// by ID.
func (s *Service) fleetNames(ctx context.Context, accountID uuid.UUID) (map[uuid.UUID]domain.Shape, map[uuid.UUID]domain.CapacityProvider, error) {
	shapes, err := s.cfg.Store.Shapes().ListAvailable(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	byShape := make(map[uuid.UUID]domain.Shape, len(shapes))
	for _, sh := range shapes {
		byShape[sh.ID] = sh
	}
	providers, err := s.providers(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	byProvider := make(map[uuid.UUID]domain.CapacityProvider, len(providers))
	for _, p := range providers {
		byProvider[p.ID] = p
	}
	return byShape, byProvider, nil
}

// providers returns the organization's providers and the instance-wide ones.
func (s *Service) providers(ctx context.Context, accountID uuid.UUID) ([]domain.CapacityProvider, error) {
	own, err := s.cfg.Store.CapacityProviders().List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	hosted, err := s.cfg.Store.CapacityProviders().List(ctx, uuid.Nil())
	if err != nil {
		return nil, err
	}
	return append(own, hosted...), nil
}

// ListShapes implements mcp.ToolService. Instance-wide shapes show as far as
// the organization's policy lets it use them, without the instance's settings.
func (s *Service) ListShapes(ctx context.Context, by domain.Actor) ([]mcp.ShapeInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	shapes, providers, err := s.fleetNames(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	policy, err := s.cfg.Store.FleetPolicies().Get(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ShapeInfo, 0, len(shapes))
	for _, sh := range shapes {
		if sh.Ownership() == domain.OwnershipHosted {
			if !policy.Allows(sh) {
				continue
			}
			sh.ProviderConfig, sh.MonthlyBudget = nil, nil
		}
		out = append(out, mcp.ShapeInfo{Shape: sh, Provider: providers[sh.ProviderID].Name, Ownership: sh.Ownership()})
	}
	slices.SortFunc(out, func(a, b mcp.ShapeInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ListProviders implements mcp.ToolService. An instance-wide provider shows
// when the organization's policy lets it use one of its shapes, with what
// its jobs need to know and nothing of how the instance is built.
func (s *Service) ListProviders(ctx context.Context, by domain.Actor, in mcp.ListProvidersInput) ([]mcp.ProviderInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	shapes, err := s.ListShapes(ctx, by)
	if err != nil {
		return nil, err
	}
	allowed := map[uuid.UUID]bool{}
	for _, sh := range shapes {
		allowed[sh.ProviderID] = true
	}
	providers, err := s.providers(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]mcp.ProviderInfo, 0, len(providers))
	for _, p := range providers {
		online := p.Online(now)
		if p.Ownership() == domain.OwnershipHosted {
			if !allowed[p.ID] {
				continue
			}
			p = hostedView(p)
		}
		out = append(out, wrapProvider(mcp.ProviderInfo{CapacityProvider: p, Online: online, Ownership: p.Ownership()}, in.WrapUntrusted))
	}
	return out, nil
}

// wrapMachine marks m's error as data: a fleet agent may have written it.
func wrapMachine(m *mcp.MachineInfo, wrap bool) {
	if wrap && m.Error != "" {
		m.Untrusted = contextpack.WrapUntrusted("error of machine "+m.Name, m.Error)
		m.Error = ""
	}
}

// reportedWords is what an agent says about itself in words. Nobody checked
// it, and it may reach another agent.
type reportedWords struct {
	Host         string                 `json:"host,omitempty"`
	CloudAccount string                 `json:"cloud_account,omitempty"`
	Region       string                 `json:"region,omitempty"`
	AgentVersion string                 `json:"agent_version,omitempty"`
	Checks       []domain.ProviderCheck `json:"checks,omitempty"`
}

// wrapFacts moves the words of c into a block marked as data, source naming
// where they come from, and leaves the structured fields. With no words the
// block is empty.
func wrapFacts(source string, c domain.ProviderCapabilities) (domain.ProviderCapabilities, string) {
	words := reportedWords{c.Host, c.CloudAccount, c.Region, c.AgentVersion, c.Checks}
	if words.Host+words.CloudAccount+words.Region+words.AgentVersion == "" && len(words.Checks) == 0 {
		return c, ""
	}
	raw, _ := json.Marshal(words) // plain strings
	c.Host, c.CloudAccount, c.Region, c.AgentVersion, c.Checks = "", "", "", "", nil
	return c, contextpack.WrapUntrusted(source, string(raw))
}

func wrapProvider(p mcp.ProviderInfo, wrap bool) mcp.ProviderInfo {
	if wrap {
		p.Capabilities, p.Untrusted = wrapFacts("fleet agent of provider "+p.Name, p.Capabilities)
	}
	return p
}

// hostedView leaves out of an instance-wide provider what says how the
// instance is built or what it may spend.
func hostedView(p domain.CapacityProvider) domain.CapacityProvider {
	c := &p.Capabilities
	c.Host, c.CloudAccount, c.Region, c.AgentVersion, c.Checks = "", "", "", "", nil
	p.Tags, p.NetworkAllow, p.Limits = nil, nil, domain.ProviderLimits{}
	return p
}

// FleetOverview implements mcp.ToolService.
func (s *Service) FleetOverview(ctx context.Context, by domain.Actor) (fleet.Overview, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return fleet.Overview{}, err
	}
	if s.cfg.Fleet == nil {
		return fleet.Overview{}, fmt.Errorf("the fleet is not configured: %w", mcp.ErrUnavailable)
	}
	visible, err := s.scope(ctx, by, domain.PermJobRead, uuid.Nil())
	if err != nil {
		return fleet.Overview{}, err
	}
	return s.cfg.Fleet.Overview(ctx, by.AccountID, visible.Has)
}

// FleetUsage implements mcp.ToolService.
func (s *Service) FleetUsage(ctx context.Context, by domain.Actor, in mcp.FleetUsageInput) (fleet.UsageReport, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return fleet.UsageReport{}, err
	}
	if s.cfg.Fleet == nil {
		return fleet.UsageReport{}, fmt.Errorf("the fleet is not configured: %w", mcp.ErrUnavailable)
	}
	from := s.cfg.Now().UTC()
	from = time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	if in.Month != "" {
		m, err := time.Parse("2006-01", in.Month)
		if err != nil {
			return fleet.UsageReport{}, fmt.Errorf("month %q must be YYYY-MM: %w", in.Month, mcp.ErrInvalidInput)
		}
		from = m
	}
	return s.cfg.Fleet.Usage(ctx, by.AccountID, from, from.AddDate(0, 1, 0), in.Records)
}

// Sizes of list_fleet_notices.
const (
	DefaultNoticeLimit = 100
	MaxNoticeLimit     = 500
)

// operator lets by at the instance's notices: an instance admin, with perm in
// its organization.
func (s *Service) operator(ctx context.Context, by domain.Actor, perm domain.Permission) error {
	if err := s.allowOrg(ctx, by, perm); err != nil {
		return err
	}
	admin, err := s.instanceAdmin(ctx, by)
	if err != nil {
		return err
	}
	if !admin {
		return fmt.Errorf("notices are for instance admins: %w", authz.ErrDenied)
	}
	return nil
}

// ListFleetNotices implements mcp.ToolService.
func (s *Service) ListFleetNotices(ctx context.Context, by domain.Actor, in mcp.ListFleetNoticesInput) ([]mcp.FleetNoticeInfo, error) {
	if err := s.operator(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	if in.Limit < 0 {
		return nil, fmt.Errorf("limit %d: %w", in.Limit, mcp.ErrInvalidInput)
	}
	if in.Limit == 0 {
		in.Limit = DefaultNoticeLimit
	}
	notices, err := s.cfg.Store.FleetNotices().List(ctx, in.All, min(in.Limit, MaxNoticeLimit))
	if err != nil {
		return nil, err
	}
	out := make([]mcp.FleetNoticeInfo, len(notices))
	for i, n := range notices {
		if out[i], err = s.noticeInfo(ctx, n, in.WrapUntrusted); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ResolveFleetNotice implements mcp.ToolService.
func (s *Service) ResolveFleetNotice(ctx context.Context, by domain.Actor, in mcp.FleetNoticeRefInput) (mcp.FleetNoticeInfo, error) {
	if err := s.operator(ctx, by, domain.PermWorkerManage); err != nil {
		return mcp.FleetNoticeInfo{}, err
	}
	id, err := uuid.Parse(in.Notice)
	if err != nil {
		return mcp.FleetNoticeInfo{}, fmt.Errorf("notice %q: %w", in.Notice, mcp.ErrNotFound)
	}
	n, err := s.cfg.Store.FleetNotices().Resolve(ctx, id, by.String())
	if err != nil {
		return mcp.FleetNoticeInfo{}, notFound(err, "notice "+in.Notice)
	}
	return s.noticeInfo(ctx, n, in.WrapUntrusted)
}

func (s *Service) noticeInfo(ctx context.Context, n domain.FleetNotice, wrap bool) (mcp.FleetNoticeInfo, error) {
	out := mcp.FleetNoticeInfo{FleetNotice: n}
	if p, err := s.cfg.Store.CapacityProviders().Get(ctx, n.ProviderID); err == nil {
		out.Provider = p.Name
	} else if !errors.Is(err, store.ErrNotFound) {
		return out, err
	}
	if n.AccountID != uuid.Nil() {
		a, err := s.cfg.Store.Accounts().Get(ctx, n.AccountID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return out, err
		}
		out.Organization = a.Slug
	}
	if wrap && n.Detail != "" {
		out.Untrusted = contextpack.WrapUntrusted("abuse check of location "+out.Provider, n.Detail)
		out.Detail = ""
	}
	return out, nil
}

// instanceAdmin reports whether by is a person flagged as an instance admin:
// instance-wide rows are theirs alone (fleet.md, Permissions).
func (s *Service) instanceAdmin(ctx context.Context, by domain.Actor) (bool, error) {
	id, ok := by.SubjectID()
	if !ok || by.Kind != domain.ActorUser || authz.Delegated(by) {
		return false, nil
	}
	u, err := s.cfg.Store.Users().Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return u.InstanceAdmin, err
}

// providerOrg says whose a new provider or join code is, and that by may make
// it: by's own organization, or an instance-wide one for an instance admin.
// Another organization looks like one that does not exist. A connected app
// makes neither: the token or code it would hold outlives disconnecting it.
func (s *Service) providerOrg(ctx context.Context, by domain.Actor, slug string) (uuid.UUID, error) {
	if err := s.authz.RefuseDelegated(ctx, by); err != nil {
		return uuid.Nil(), err
	}
	if err := s.allowOrg(ctx, by, domain.PermWorkerManage); err != nil {
		return uuid.Nil(), err
	}
	id, err := fleet.ProviderOrganization(ctx, s.cfg.Store, slug)
	if errors.Is(err, fleet.ErrOrgRequired) {
		return uuid.Nil(), fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if err != nil {
		return uuid.Nil(), notFound(err, "organization "+slug)
	}
	switch id {
	case by.AccountID:
		return id, nil
	case uuid.Nil():
		admin, err := s.instanceAdmin(ctx, by)
		if err != nil {
			return uuid.Nil(), err
		}
		if !admin {
			return uuid.Nil(), fmt.Errorf("an instance-wide location needs an instance admin: %w", authz.ErrDenied)
		}
		return id, nil
	}
	return uuid.Nil(), fmt.Errorf("organization %s: %w", slug, mcp.ErrNotFound)
}

// mayManage reports whether by may manage rows of account, by's organization
// or, for an instance admin, the instance-wide ones.
func (s *Service) mayManage(ctx context.Context, by domain.Actor, account uuid.UUID) (bool, error) {
	if account == by.AccountID {
		return true, nil
	}
	if account != uuid.Nil() {
		return false, nil
	}
	return s.instanceAdmin(ctx, by)
}

// CreateProvider implements mcp.ToolService.
func (s *Service) CreateProvider(ctx context.Context, by domain.Actor, in mcp.CreateProviderInput) (mcp.CreateProviderOutput, error) {
	var out mcp.CreateProviderOutput
	account, err := s.providerOrg(ctx, by, in.Org)
	if err != nil {
		return out, err
	}
	token, hash := fleet.NewProviderToken()
	p := domain.CapacityProvider{AccountID: account, Name: in.Name, Kind: in.Kind, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, TokenHash: hash}
	if err := s.addProvider(ctx, by, &p, "provider.created", nil); err != nil {
		return out, err
	}
	return mcp.CreateProviderOutput{Provider: providerInfo(p, s.cfg.Now()), Token: token}, nil
}

// addProvider stores p and audits it, with then in the same transaction.
func (s *Service) addProvider(ctx context.Context, by domain.Actor, p *domain.CapacityProvider, action string, then func(ctx context.Context, tx store.Store) error) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	hosted := s.cfg.Server != nil && s.cfg.Server.Fleet.Hosted
	if err := fleet.RequireHostedKind(hosted, *p); err != nil {
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if err := fleet.RequirePaidPlan(ctx, s.cfg.Store, hosted, *p); err != nil {
		return err
	}
	err := s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.CapacityProviders().Create(ctx, p); err != nil {
			return err
		}
		if then != nil {
			if err := then(ctx, tx); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, by, uuid.Nil(), action, "provider", p.ID, map[string]any{"name": p.Name, "kind": p.Kind, "ownership": p.Ownership(), "host": p.Capabilities.Host})
	})
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("a provider named %s exists already, or the code changed: %w", p.Name, mcp.ErrConflict)
	}
	return err
}

func providerInfo(p domain.CapacityProvider, now time.Time) mcp.ProviderInfo {
	return mcp.ProviderInfo{CapacityProvider: p, Online: p.Online(now), Ownership: p.Ownership()}
}

// RevokeProvider implements mcp.ToolService. Another organization's provider
// is not found.
func (s *Service) RevokeProvider(ctx context.Context, by domain.Actor, in mcp.ProviderRefInput) (mcp.ProviderInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerManage); err != nil {
		return mcp.ProviderInfo{}, err
	}
	all, err := s.providers(ctx, by.AccountID)
	if err != nil {
		return mcp.ProviderInfo{}, err
	}
	admin, err := s.instanceAdmin(ctx, by)
	if err != nil {
		return mcp.ProviderInfo{}, err
	}
	all = slices.DeleteFunc(all, func(p domain.CapacityProvider) bool { return p.AccountID != by.AccountID && !admin })
	p, err := store.FindActive(in.Provider, all, func(p domain.CapacityProvider) (uuid.UUID, string, bool) {
		return p.ID, p.Name, p.Status == domain.ProviderRevoked
	})
	if err != nil {
		return mcp.ProviderInfo{}, fmt.Errorf("provider %s: %w", in.Provider, mcp.ErrNotFound)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.CapacityProviders().SetStatus(ctx, p.ID, domain.ProviderRevoked); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "provider.revoked", "provider", p.ID, map[string]any{"name": p.Name})
	})
	if err != nil {
		return mcp.ProviderInfo{}, err
	}
	p.Status = domain.ProviderRevoked
	return wrapProvider(providerInfo(p, s.cfg.Now()), in.WrapUntrusted), nil
}

// CreateJoinCode implements mcp.ToolService.
func (s *Service) CreateJoinCode(ctx context.Context, by domain.Actor, in mcp.CreateJoinCodeInput) (mcp.CreateJoinCodeOutput, error) {
	var out mcp.CreateJoinCodeOutput
	account, err := s.providerOrg(ctx, by, in.Org)
	if err != nil {
		return out, err
	}
	ttl := domain.JoinCodeTTL
	if in.Cloud {
		ttl = domain.JoinCodeCloudTTL
	}
	code, hash := fleet.NewJoinCode()
	c := domain.ProviderJoinCode{AccountID: account, CodeHash: hash, ExpiresAt: s.cfg.Now().Add(ttl)}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ProviderJoinCodes().Create(ctx, &c); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "provider.join_code_created", "join_code", c.ID, map[string]any{"expires_at": c.ExpiresAt})
	})
	if err != nil {
		return out, err
	}
	return mcp.CreateJoinCodeOutput{JoinCode: s.joinInfo(c, false), Code: code}, nil
}

// joinInfo says where a code stands. Nothing of a code's secrets leaves: its
// hashes have no JSON name.
func (s *Service) joinInfo(c domain.ProviderJoinCode, wrap bool) mcp.JoinCodeInfo {
	info := mcp.JoinCodeInfo{ProviderJoinCode: c}
	switch {
	case !c.UsedAt.IsZero():
		info.Status = mcp.JoinUsed
	case !c.Usable(s.cfg.Now()):
		info.Status = mcp.JoinExpired
	case c.ProviderID != uuid.Nil():
		info.Status = mcp.JoinConfirmed
	case len(c.ClaimHash) != 0:
		info.Status = mcp.JoinClaimed
	default:
		info.Status = mcp.JoinWaiting
	}
	if len(c.ClaimHash) != 0 {
		info.FactsHash = fleet.FactsHash(c.Kind, c.Facts)
	}
	if wrap {
		info.Facts, info.Untrusted = wrapFacts("fleet agent "+c.ID.String(), c.Facts)
	}
	return info
}

// ListJoinCodes implements mcp.ToolService: the organization's codes, and the
// instance-wide ones for an instance admin.
func (s *Service) ListJoinCodes(ctx context.Context, by domain.Actor, in mcp.ListJoinCodesInput) ([]mcp.JoinCodeInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerManage); err != nil {
		return nil, err
	}
	codes, err := s.cfg.Store.ProviderJoinCodes().List(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	admin, err := s.instanceAdmin(ctx, by)
	if err != nil {
		return nil, err
	}
	if admin {
		wide, err := s.cfg.Store.ProviderJoinCodes().List(ctx, uuid.Nil())
		if err != nil {
			return nil, err
		}
		codes = append(codes, wide...)
	}
	out := make([]mcp.JoinCodeInfo, 0, len(codes))
	for _, c := range codes {
		out = append(out, s.joinInfo(c, in.WrapUntrusted))
	}
	return out, nil
}

// ConfirmJoin implements mcp.ToolService. The provider has the code's
// organization, the kind and facts the agent reported and no token: the agent
// gets that at its next call. The caller names the report it read, and the
// provider gets those facts, whatever the agent sends meanwhile.
func (s *Service) ConfirmJoin(ctx context.Context, by domain.Actor, in mcp.ConfirmJoinInput) (mcp.ProviderInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerManage); err != nil {
		return mcp.ProviderInfo{}, err
	}
	id, err := uuid.Parse(in.Join)
	if err != nil {
		return mcp.ProviderInfo{}, fmt.Errorf("join %q is not a UUID: %w", in.Join, mcp.ErrInvalidInput)
	}
	c, err := s.cfg.Store.ProviderJoinCodes().Get(ctx, id)
	if err != nil {
		return mcp.ProviderInfo{}, notFound(err, "join code "+in.Join)
	}
	ok, err := s.mayManage(ctx, by, c.AccountID)
	if err != nil {
		return mcp.ProviderInfo{}, err
	}
	if !ok {
		return mcp.ProviderInfo{}, fmt.Errorf("join code %s: %w", in.Join, mcp.ErrNotFound)
	}
	info := s.joinInfo(c, false)
	if info.Status != mcp.JoinClaimed {
		return mcp.ProviderInfo{}, fmt.Errorf("join code %s is %s; only one an agent claimed can be confirmed: %w", in.Join, info.Status, mcp.ErrConflict)
	}
	if in.FactsHash == "" {
		return mcp.ProviderInfo{}, fmt.Errorf("facts_hash of the report you read is required: %w", mcp.ErrInvalidInput)
	}
	if in.FactsHash != info.FactsHash {
		return mcp.ProviderInfo{}, fmt.Errorf("the agent's report changed since you read it; read it again: %w", mcp.ErrConflict)
	}
	p := domain.CapacityProvider{AccountID: c.AccountID, Name: in.Name, Kind: c.Kind, Mode: domain.ProviderModeAgent, Status: domain.ProviderPending, Capabilities: c.Facts}
	err = s.addProvider(ctx, by, &p, "provider.confirmed", func(ctx context.Context, tx store.Store) error {
		return tx.ProviderJoinCodes().Confirm(ctx, c.ID, p.ID)
	})
	if err != nil {
		return mcp.ProviderInfo{}, err
	}
	return wrapProvider(providerInfo(p, s.cfg.Now()), in.WrapUntrusted), nil
}
