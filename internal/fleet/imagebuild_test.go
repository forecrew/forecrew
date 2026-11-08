package fleet_test

import (
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/store"
)

// windowsProvider adds an active Hyper-V location served by an agent.
func (e *env) windowsProvider(account uuid.UUID) domain.CapacityProvider {
	e.t.Helper()
	_, hash := fleet.NewProviderToken()
	p := domain.CapacityProvider{AccountID: account, Name: "win-" + uuid.New().String()[:8], Kind: domain.ProviderHyperV, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, TokenHash: hash}
	must(e.t, e.st.CapacityProviders().Create(e.ctx, &p))
	return p
}

func (e *env) build(p domain.CapacityProvider) domain.ImageBuild {
	e.t.Helper()
	b := domain.ImageBuild{AccountID: p.AccountID, ProviderID: p.ID, Version: "2026.11.1", ISO: `D:\isos\server.iso`, ISOChecksum: "sha256:" + strings.Repeat("a", 64),
		ChocolateySHA256: strings.Repeat("b", 64), LicenseConfirmedBy: "user:ana"}
	must(e.t, e.st.ImageBuilds().Create(e.ctx, &b))
	return b
}

func TestCheckBuildable(t *testing.T) {
	win := domain.CapacityProvider{Kind: domain.ProviderHyperV, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive}
	if err := fleet.CheckBuildable(win); err != nil {
		t.Fatal(err)
	}
	prox := win
	prox.Kind = domain.ProviderProxmox
	if err := fleet.CheckBuildable(prox); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(p *domain.CapacityProvider){
		"kind":    func(p *domain.CapacityProvider) { p.Kind = domain.ProviderAWS },
		"direct":  func(p *domain.CapacityProvider) { p.Mode = domain.ProviderModeDirect },
		"paused":  func(p *domain.CapacityProvider) { p.Status = domain.ProviderPaused },
		"pending": func(p *domain.CapacityProvider) { p.Status = domain.ProviderPending },
	} {
		p := win
		mutate(&p)
		if err := fleet.CheckBuildable(p); !errors.Is(err, fleet.ErrBuildNotAvailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDesiredImageBuilds(t *testing.T) {
	e := newEnv(t)
	account, _ := e.org("a")
	p := e.windowsProvider(account)
	got, err := fleet.DesiredImageBuilds(e.ctx, e.st, p)
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("none yet: %+v", got)
	}
	d0, err := e.c.Desired(e.ctx, p, nil)
	must(t, err)
	b := e.build(p)
	got, err = fleet.DesiredImageBuilds(e.ctx, e.st, p)
	must(t, err)
	if len(got) != 1 || got[0].ID != b.ID || got[0].State != domain.ImageBuildRequested || !got[0].LicenseConfirmed || got[0].ISO != b.ISO {
		t.Fatalf("desired %+v", got)
	}
	d1, err := e.c.Desired(e.ctx, p, nil)
	must(t, err)
	if d1.Generation == d0.Generation || len(d1.ImageBuilds) != 1 {
		t.Fatalf("a new build changes the generation: %s %s %+v", d0.Generation, d1.Generation, d1.ImageBuilds)
	}
	// A paused location starts nothing new, but keeps the build it is doing.
	p.Status = domain.ProviderPaused
	if got, _ = fleet.DesiredImageBuilds(e.ctx, e.st, p); len(got) != 0 {
		t.Fatalf("paused %+v", got)
	}
	_, err = fleet.ReportImageBuild(e.ctx, e.st, e.clock.Now(), p, b.ID, fleet.ImageBuildReport{State: domain.ImageBuildRunning, Step: "checking", Percent: 5})
	must(t, err)
	if got, _ = fleet.DesiredImageBuilds(e.ctx, e.st, p); len(got) != 1 || got[0].State != domain.ImageBuildRunning {
		t.Fatalf("running while paused %+v", got)
	}
}

func TestReportImageBuild(t *testing.T) {
	e := newEnv(t)
	account, _ := e.org("a")
	p := e.windowsProvider(account)
	other := e.windowsProvider(account)
	b := e.build(p)
	now := e.clock.Now()
	report := func(r fleet.ImageBuildReport) (domain.ImageBuild, error) {
		return fleet.ReportImageBuild(e.ctx, e.st, now, p, b.ID, r)
	}

	if _, err := fleet.ReportImageBuild(e.ctx, e.st, now, other, b.ID, fleet.ImageBuildReport{State: domain.ImageBuildRunning}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another provider: %v", err)
	}
	if _, err := fleet.ReportImageBuild(e.ctx, e.st, now, p, uuid.New(), fleet.ImageBuildReport{State: domain.ImageBuildRunning}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	for name, r := range map[string]fleet.ImageBuildReport{
		"requested":     {State: domain.ImageBuildRequested},
		"percent":       {State: domain.ImageBuildRunning, Percent: 101},
		"control":       {State: domain.ImageBuildRunning, Step: "a\nb"},
		"long error":    {State: domain.ImageBuildFailed, Error: strings.Repeat("x", 2001)},
		"done no files": {State: domain.ImageBuildDone},
		"done no hash":  {State: domain.ImageBuildDone, Manifest: "m.json", Digest: "md5:1"},
		"failed no why": {State: domain.ImageBuildFailed},
	} {
		if _, err := report(r); !errors.Is(err, fleet.ErrBadReport) {
			t.Errorf("%s: %v", name, err)
		}
	}

	got, err := report(fleet.ImageBuildReport{State: domain.ImageBuildRunning, Step: "building the image: about an hour", Percent: 15})
	must(t, err)
	if got.State != domain.ImageBuildRunning || got.Percent != 15 || got.StartedAt.IsZero() || !got.FinishedAt.IsZero() {
		t.Fatalf("running %+v", got)
	}
	got, err = report(fleet.ImageBuildReport{State: domain.ImageBuildDone, Step: "done", Percent: 3, Manifest: `D:\b\m.json`, Digest: digest})
	must(t, err)
	if got.State != domain.ImageBuildDone || got.Percent != 100 || got.Manifest == "" || got.Digest != digest || got.FinishedAt.IsZero() {
		t.Fatalf("done %+v", got)
	}
	if _, err := report(fleet.ImageBuildReport{State: domain.ImageBuildDone, Step: "done", Manifest: `D:\b\m.json`, Digest: digest}); err != nil {
		t.Errorf("the end again: %v", err)
	}
	if _, err := report(fleet.ImageBuildReport{State: domain.ImageBuildFailed, Error: "late"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("another end: %v", err)
	}
	// The end is in the organization's log.
	entries, err := e.st.Audit().List(e.ctx, account, store.AuditFilter{})
	must(t, err)
	var found bool
	for _, a := range entries {
		found = found || (a.Action == fleet.ActionImageBuildFinished && a.SubjectID == b.ID)
	}
	if !found {
		t.Errorf("no audit entry: %+v", entries)
	}
}

func TestReportImageBuildFailureIsCleaned(t *testing.T) {
	e := newEnv(t)
	account, _ := e.org("a")
	p := e.windowsProvider(account)
	b := e.build(p)
	got, err := fleet.ReportImageBuild(e.ctx, e.st, e.clock.Now(), p, b.ID, fleet.ImageBuildReport{State: domain.ImageBuildFailed, Error: "packer: token fcp_" + strings.Repeat("A", 43) + " rejected"})
	must(t, err)
	if got.State != domain.ImageBuildFailed || strings.Contains(got.Error, "fcp_") || !strings.Contains(got.Error, "[redacted]") {
		t.Fatalf("failed %+v", got)
	}
}

func TestExpireImageBuilds(t *testing.T) {
	e := newEnv(t)
	account, _ := e.org("a")
	p := e.windowsProvider(account)
	b := e.build(p)
	must(t, fleet.ExpireImageBuilds(e.ctx, e.st, e.clock.Now(), p.ID))
	if got, _ := e.st.ImageBuilds().Get(e.ctx, b.ID); got.State != domain.ImageBuildRequested {
		t.Fatalf("too early: %+v", got)
	}
	// Not taken in time.
	e.clock.Advance(fleet.BuildPickupWithin + time.Minute)
	must(t, fleet.ExpireImageBuilds(e.ctx, e.st, e.clock.Now(), p.ID))
	got, _ := e.st.ImageBuilds().Get(e.ctx, b.ID)
	if got.State != domain.ImageBuildFailed || !strings.Contains(got.Error, "did not take the build") || got.FinishedAt.IsZero() {
		t.Fatalf("requested too long: %+v", got)
	}
	// Taken, then silent.
	b = e.build(p)
	_, err := fleet.ReportImageBuild(e.ctx, e.st, e.clock.Now(), p, b.ID, fleet.ImageBuildReport{State: domain.ImageBuildRunning, Step: "building", Percent: 15})
	must(t, err)
	e.clock.Advance(fleet.BuildSilentFor - time.Minute)
	must(t, fleet.ExpireImageBuilds(e.ctx, e.st, e.clock.Now(), p.ID))
	if got, _ = e.st.ImageBuilds().Get(e.ctx, b.ID); got.State != domain.ImageBuildRunning {
		t.Fatalf("still reporting: %+v", got)
	}
	e.clock.Advance(2 * time.Minute)
	must(t, fleet.ExpireImageBuilds(e.ctx, e.st, e.clock.Now(), p.ID))
	if got, _ = e.st.ImageBuilds().Get(e.ctx, b.ID); got.State != domain.ImageBuildFailed || !strings.Contains(got.Error, "stopped reporting") {
		t.Fatalf("silent: %+v", got)
	}
}
