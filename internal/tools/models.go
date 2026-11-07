package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	cfgres "github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/models"
)

// modelChecks returns the model checker, or why there is none.
func (s *Service) modelChecks() (*models.Checker, error) {
	if s.cfg.Models == nil {
		return nil, fmt.Errorf("model checks are not running on this server: %w", mcp.ErrUnavailable)
	}
	return s.cfg.Models, nil
}

// checkError maps what the checker says to the tools' errors.
func checkError(err error) error {
	switch {
	case errors.Is(err, models.ErrUnknownConnection):
		return fmt.Errorf("%v: %w", err, mcp.ErrNotFound)
	case errors.Is(err, models.ErrNoProject), errors.Is(err, jobs.ErrInvalid), errors.Is(err, config.ErrNotAllowed):
		return fmt.Errorf("%v: %w", err, mcp.ErrUnavailable)
	}
	return err
}

// checkView is a check as the caller sees it: what the provider and the
// agent said is wrapped as untrusted for an MCP client.
func checkView(c domain.ModelCheck, wrap bool) mcp.ModelCheckView {
	v := mcp.ModelCheckView{ModelCheck: c}
	if !wrap {
		return v
	}
	var said []string
	if c.Key != nil && len(c.Key.Models) > 0 {
		k := *c.Key
		said, k.Models = append(said, "models: "+strings.Join(k.Models, ", ")), nil
		v.Key = &k
	}
	if c.Test != nil && c.Test.Error != "" {
		t := *c.Test
		said, t.Error = append(said, "error: "+t.Error), ""
		v.Test = &t
	}
	if len(said) > 0 {
		v.Untrusted = contextpack.WrapUntrusted("what the provider and the agent said", strings.Join(said, "\n"))
	}
	return v
}

// ModelsCheck implements mcp.ToolService.
func (s *Service) ModelsCheck(ctx context.Context, by domain.Actor, in mcp.ModelConnectionRefInput) (mcp.ModelCheckView, error) {
	if err := s.allowOrg(ctx, by, domain.PermModelsManage); err != nil {
		return mcp.ModelCheckView{}, err
	}
	c, err := s.modelChecks()
	if err != nil {
		return mcp.ModelCheckView{}, err
	}
	row, err := c.Check(ctx, by, in.Connection)
	return checkView(row, in.WrapUntrusted), checkError(err)
}

// ModelsTest implements mcp.ToolService: the estimate, and with Run the
// test.
func (s *Service) ModelsTest(ctx context.Context, by domain.Actor, in mcp.ModelsTestInput) (mcp.ModelsTestOutput, error) {
	if err := s.allowOrg(ctx, by, domain.PermModelsManage); err != nil {
		return mcp.ModelsTestOutput{}, err
	}
	c, err := s.modelChecks()
	if err != nil {
		return mcp.ModelsTestOutput{}, err
	}
	if strings.TrimSpace(in.Model) == "" {
		return mcp.ModelsTestOutput{}, fmt.Errorf("model is required: %w", mcp.ErrInvalidInput)
	}
	conn, err := s.modelConnection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return mcp.ModelsTestOutput{}, err
	}
	e := c.EstimateTest(conn, in.Model)
	out := mcp.ModelsTestOutput{Connection: conn.Name, Model: in.Model, MaxTurns: models.TestMaxTurns, TimeoutSeconds: int(models.TestTimeout.Seconds()),
		Estimate: mcp.ModelsEstimate{InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CostUSD: e.CostUSD, Basis: e.Basis}}
	if !in.Run {
		return out, nil
	}
	row, err := c.Test(ctx, by, conn.Name, in.Model)
	if err != nil {
		return out, checkError(err)
	}
	v := checkView(row, in.WrapUntrusted)
	out.Test = &v
	return out, nil
}

// modelConnection returns the organization's connection called name.
func (s *Service) modelConnection(ctx context.Context, accountID uuid.UUID, name string) (domain.ModelConnection, error) {
	rows, err := s.cfg.Store.ModelConnections().List(ctx, accountID)
	if err != nil {
		return domain.ModelConnection{}, err
	}
	for _, c := range rows {
		if c.Name == name {
			return c, nil
		}
	}
	return domain.ModelConnection{}, fmt.Errorf("model connection %q: %w", name, mcp.ErrNotFound)
}

// ModelsResults implements mcp.ToolService.
func (s *Service) ModelsResults(ctx context.Context, by domain.Actor, in mcp.ModelsResultsInput) ([]mcp.ModelCheckView, error) {
	if err := s.allowOrg(ctx, by, domain.PermModelsRead); err != nil {
		return nil, err
	}
	rows, err := s.cfg.Store.ModelChecks().List(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := []mcp.ModelCheckView{}
	for _, r := range rows {
		if in.Connection == "" || r.Connection == in.Connection {
			out = append(out, checkView(r, in.WrapUntrusted))
		}
	}
	return out, nil
}

// ModelsSuggest implements mcp.ToolService.
func (s *Service) ModelsSuggest(ctx context.Context, by domain.Actor, in mcp.ModelsSuggestInput) (mcp.ModelsSuggestion, error) {
	if err := s.allowOrg(ctx, by, domain.PermModelsRead); err != nil {
		return mcp.ModelsSuggestion{}, err
	}
	return s.suggest(ctx, by, in.Preset, cfgres.PlanRequest{Source: orForm(in.Source)})
}

// suggest makes a suggestion and its plan, as req says; the assistant asks
// through the same function.
func (s *Service) suggest(ctx context.Context, by domain.Actor, preset string, req cfgres.PlanRequest) (mcp.ModelsSuggestion, error) {
	cat, err := models.DefaultCatalog()
	if err != nil {
		return mcp.ModelsSuggestion{}, err
	}
	conns, err := s.cfg.Store.ModelConnections().List(ctx, by.AccountID)
	if err != nil {
		return mcp.ModelsSuggestion{}, err
	}
	checks, err := s.cfg.Store.ModelChecks().List(ctx, by.AccountID)
	if err != nil {
		return mcp.ModelsSuggestion{}, err
	}
	var needs []models.RoleNeed
	if s.cfg.RoleNeeds != nil {
		needs = s.cfg.RoleNeeds()
	}
	allow := s.cfg.Server != nil && s.cfg.Server.Policy.AllowSameVendorReview
	candidates, notes := models.Candidates(cat, conns, checks)
	sg, err := models.Suggest(candidates, needs, preset, allow)
	if err != nil {
		return mcp.ModelsSuggestion{}, fmt.Errorf("%v: %w", err, mcp.ErrInvalidInput)
	}
	out := mcp.ModelsSuggestion{Preset: sg.Preset, Tiers: sg.Tiers, Roles: sg.Roles, Notes: append(notes, sg.Notes...)}
	if len(sg.Roles) == 0 {
		return out, nil
	}
	rows, err := s.cfg.Store.ModelAssignments().List(ctx, by.AccountID)
	if err != nil {
		return out, err
	}
	cur := models.NewState(nil, rows).Org
	byName := map[string]domain.ModelConnection{}
	for _, c := range conns {
		byName[c.Name] = c
	}
	tiers, roles, merged := models.Merge(cur, sg, byName)
	out.Notes = append(out.Notes, merged...)
	spec := map[string]any{"tiers": tiers, "roles": roles}
	if cur.ProjectOverrides != nil {
		spec["project_overrides"] = *cur.ProjectOverrides
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return out, err
	}
	req.Title = "Models suggested (" + sg.Preset + ")"
	req.Changes = []mcp.ConfigWanted{{Kind: cfgres.KindModelAssignment, Name: "organization", Spec: raw}}
	p, err := s.config.Plan(ctx, by, req)
	v, err := s.planView(ctx, by, p, err)
	if err != nil {
		return out, err
	}
	if v.ID != uuid.Nil() {
		out.Plan = &v
	}
	return out, nil
}

// checkSaved starts the key check of each connection an applied plan created
// or changed and left on. A check that cannot start is logged: the plan is
// applied anyway, and the check can be run again.
func (s *Service) checkSaved(ctx context.Context, by domain.Actor, p mcp.ConfigPlanView) {
	if s.cfg.Models == nil {
		return
	}
	for _, c := range p.Changes {
		if c.Kind != cfgres.KindModelConnection || c.Op == domain.ConfigDelete {
			continue
		}
		var spec struct {
			Enabled bool `json:"enabled"`
		}
		if json.Unmarshal(c.New, &spec) != nil || !spec.Enabled {
			continue
		}
		if _, err := s.cfg.Models.Check(ctx, by, c.Name); err != nil {
			s.cfg.Logger.WarnContext(ctx, "key check not started", "connection", c.Name, "error", err)
		}
	}
}
