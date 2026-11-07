package tools

import (
	"context"
	"fmt"
	"strings"
	"uuid"

	cfgres "github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// MaxPlanList is the default size of config_plans.
const MaxPlanList = 100

func planID(ref string) (uuid.UUID, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return id, fmt.Errorf("plan %q is not a UUID: %w", ref, mcp.ErrInvalidInput)
	}
	return id, nil
}

// orForm is the source of a plan whose entry point named none.
func orForm(s domain.ConfigPlanSource) domain.ConfigPlanSource {
	if s == "" {
		return domain.ConfigFromForm
	}
	return s
}

// planView returns a freshly made plan as the caller sees it; a plan with no
// change was not stored and has no ID.
func (s *Service) planView(ctx context.Context, by domain.Actor, p domain.ConfigPlan, err error) (mcp.ConfigPlanView, error) {
	if err != nil {
		return mcp.ConfigPlanView{}, err
	}
	if p.ID == uuid.Nil() {
		return mcp.ConfigPlanView{ConfigPlan: p}, nil
	}
	return s.config.GetPlan(ctx, by, p.ID)
}

// ConfigSchema implements mcp.ToolService.
func (s *Service) ConfigSchema(_ context.Context, _ domain.Actor, in mcp.ConfigKindInput) ([]mcp.ConfigSchema, error) {
	return s.config.Schema(in.Kind)
}

// ConfigGet implements mcp.ToolService.
func (s *Service) ConfigGet(ctx context.Context, by domain.Actor, in mcp.ConfigGetInput) ([]mcp.ConfigResource, error) {
	if err := s.allowOrg(ctx, by, domain.PermConfigRead); err != nil {
		return nil, err
	}
	return s.config.Get(ctx, by, in.Kind, in.Name)
}

// ConfigPlan implements mcp.ToolService.
func (s *Service) ConfigPlan(ctx context.Context, by domain.Actor, in mcp.ConfigPlanInput) (mcp.ConfigPlanView, error) {
	p, err := s.config.Plan(ctx, by, cfgres.PlanRequest{Title: in.Title, Request: in.Request, Source: orForm(in.Source), Changes: in.Changes})
	return s.planView(ctx, by, p, err)
}

// ConfigApply implements mcp.ToolService.
func (s *Service) ConfigApply(ctx context.Context, by domain.Actor, in mcp.ConfigApplyInput) (mcp.ConfigApplied, error) {
	id, err := planID(in.Plan)
	if err != nil {
		return mcp.ConfigApplied{}, err
	}
	out, err := s.config.Apply(ctx, by, cfgres.Confirmation{Plan: id, Browser: in.Browser, Word: in.Word})
	if err == nil {
		s.checkSaved(ctx, by, out.Plan)
	}
	return out, err
}

// ConfigRequestApproval implements mcp.ToolService.
func (s *Service) ConfigRequestApproval(ctx context.Context, by domain.Actor, in mcp.ConfigPlanRefInput) (mcp.ConfigConfirmation, error) {
	id, err := planID(in.Plan)
	if err != nil {
		return mcp.ConfigConfirmation{}, err
	}
	p, err := s.config.RequestApproval(ctx, by, id)
	if err != nil {
		return mcp.ConfigConfirmation{}, err
	}
	out := mcp.ConfigConfirmation{Status: "confirmation_required", Plan: p, Message: "Nothing has changed. A person confirms this plan in the Inbox of the web UI."}
	if s.cfg.PublicURL != "" {
		out.ConfirmURL = strings.TrimRight(s.cfg.PublicURL, "/") + "/inbox?plan=" + p.ID.String()
		out.Message += " Open " + out.ConfirmURL
	}
	return out, nil
}

// ConfigRevert implements mcp.ToolService.
func (s *Service) ConfigRevert(ctx context.Context, by domain.Actor, in mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
	id, err := planID(in.Plan)
	if err != nil {
		return mcp.ConfigPlanView{}, err
	}
	p, err := s.config.Revert(ctx, by, id, orForm(in.Source), "")
	return s.planView(ctx, by, p, err)
}

// ConfigExport implements mcp.ToolService.
func (s *Service) ConfigExport(ctx context.Context, by domain.Actor) (mcp.ConfigExport, error) {
	if err := s.allowOrg(ctx, by, domain.PermConfigRead); err != nil {
		return mcp.ConfigExport{}, err
	}
	out, err := s.config.Export(ctx, by)
	return mcp.ConfigExport{YAML: out}, err
}

// ConfigImport implements mcp.ToolService.
func (s *Service) ConfigImport(ctx context.Context, by domain.Actor, in mcp.ConfigImportInput) (mcp.ConfigPlanView, error) {
	source := in.Source
	if source == "" {
		source = domain.ConfigFromYAML
	}
	p, err := s.config.Import(ctx, by, in.YAML, source, "")
	return s.planView(ctx, by, p, err)
}

// ConfigPlans implements mcp.ToolService.
func (s *Service) ConfigPlans(ctx context.Context, by domain.Actor, in mcp.ConfigPlansInput) ([]mcp.ConfigPlanView, error) {
	if err := s.allowOrg(ctx, by, domain.PermConfigRead); err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 || limit > MaxPlanList {
		limit = MaxPlanList
	}
	return s.config.ListPlans(ctx, by, in.Inbox, limit)
}

// ConfigGetPlan implements mcp.ToolService.
func (s *Service) ConfigGetPlan(ctx context.Context, by domain.Actor, in mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
	if err := s.allowOrg(ctx, by, domain.PermConfigRead); err != nil {
		return mcp.ConfigPlanView{}, err
	}
	id, err := planID(in.Plan)
	if err != nil {
		return mcp.ConfigPlanView{}, err
	}
	return s.config.GetPlan(ctx, by, id)
}

// ConfigCancel implements mcp.ToolService.
func (s *Service) ConfigCancel(ctx context.Context, by domain.Actor, in mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
	id, err := planID(in.Plan)
	if err != nil {
		return mcp.ConfigPlanView{}, err
	}
	return s.config.Cancel(ctx, by, id)
}

// ConfigPutSecret implements mcp.ToolService.
func (s *Service) ConfigPutSecret(ctx context.Context, by domain.Actor, in mcp.ConfigSecretInput) (mcp.ConfigPlanView, error) {
	id, err := planID(in.Plan)
	if err != nil {
		return mcp.ConfigPlanView{}, err
	}
	return s.config.PutSecret(ctx, by, id, in.Secret, []byte(in.Value))
}

// RunConfigSweep drops what expired plans left until ctx is done.
func (s *Service) RunConfigSweep(ctx context.Context) {
	s.config.RunSweep(ctx, cfgres.SweepEvery)
}
