package tools_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tools"
)

// fleetRows adds a provider and a shape of account (zero: instance-wide).
func fleetRows(t *testing.T, st *testkit.Store, account uuid.UUID, name string) (domain.CapacityProvider, domain.Shape) {
	t.Helper()
	ctx := t.Context()
	p := domain.CapacityProvider{AccountID: account, Name: name, Kind: domain.ProviderFake, Mode: domain.ProviderModeDirect, Status: domain.ProviderActive,
		NetworkAllow: []string{"10.0.0.1"}, Tags: map[string]string{"site": "secret-site"},
		Capabilities: domain.ProviderCapabilities{Host: "host-1", CloudAccount: "1234", Checks: []domain.ProviderCheck{{Name: "kvm", Status: "ok"}}, MaxCPU: 8}}
	must(t, st.CapacityProviders().Create(ctx, &p))
	must(t, st.CapacityProviders().Seen(ctx, p.ID, nil))
	s := domain.Shape{AccountID: account, ProviderID: p.ID, Name: name + "-small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20,
		Image: "linux", ProviderConfig: map[string]any{"subnet": "s-1"}, Enabled: true, Max: 5}
	must(t, st.Shapes().Create(ctx, &s))
	return p, s
}

func TestFleetLists(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	own, shape := fleetRows(t, f.st, f.account.ID, "home")
	hosted, hostedShape := fleetRows(t, f.st, uuid.Nil(), "cloud")
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	fleetRows(t, f.st, other.ID, "theirs")

	providers, err := f.svc.ListProviders(ctx, f.by)
	must(t, err)
	if len(providers) != 2 {
		t.Fatalf("providers = %+v", providers)
	}
	for _, p := range providers {
		switch p.ID {
		case own.ID:
			if p.Ownership != domain.OwnershipOwn || p.Capabilities.Host != "host-1" || len(p.Tags) != 1 {
				t.Fatalf("own = %+v", p)
			}
		case hosted.ID:
			c := p.Capabilities
			if p.Ownership != domain.OwnershipHosted || c.Host != "" || c.CloudAccount != "" || c.Checks != nil || p.Tags != nil || p.NetworkAllow != nil || c.MaxCPU != 8 {
				t.Fatalf("hosted = %+v", p)
			}
		default:
			t.Fatalf("another organization's provider: %+v", p)
		}
	}

	shapes, err := f.svc.ListShapes(ctx, f.by)
	must(t, err)
	if len(shapes) != 2 || shapes[0].ID != hostedShape.ID || shapes[1].ID != shape.ID {
		t.Fatalf("shapes = %+v", shapes)
	}
	if shapes[0].ProviderConfig != nil || shapes[0].Provider != "cloud" || shapes[1].ProviderConfig == nil || shapes[1].Ownership != domain.OwnershipOwn {
		t.Fatalf("shapes = %+v", shapes)
	}

}

func TestListMachines(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	own, shape := fleetRows(t, f.st, f.account.ID, "home")
	hosted, hostedShape := fleetRows(t, f.st, uuid.Nil(), "cloud")
	m := domain.Machine{AccountID: f.account.ID, ProviderID: own.ID, ShapeID: shape.ID}
	must(t, f.st.Machines().Create(ctx, &m))
	gone := domain.Machine{AccountID: f.account.ID, ProviderID: own.ID, ShapeID: shape.ID}
	must(t, f.st.Machines().Create(ctx, &gone))
	must(t, f.st.Machines().UpdateState(ctx, gone.ID, domain.MachineRequested, domain.MachineDestroyed, ""))
	warm := domain.Machine{ProviderID: hosted.ID, ShapeID: hostedShape.ID} // nobody's yet
	must(t, f.st.Machines().Create(ctx, &warm))
	j := f.newJob(t, "developer", domain.Goal{}, domain.Task{}, 0)
	_, err := f.st.Machines().TakeJob(ctx, m.ID, j.ID, f.clock.Now())
	must(t, err)

	list, err := f.svc.ListMachines(ctx, f.by, mcp.ListMachinesInput{})
	must(t, err)
	if len(list) != 1 || list[0].ID != m.ID || list[0].Shape != "home-small" || list[0].Provider != "home" || list[0].JobRef != j.Ref() || list[0].Name == "" {
		t.Fatalf("machines = %+v", list)
	}
	list, err = f.svc.ListMachines(ctx, f.by, mcp.ListMachinesInput{State: "destroyed", Limit: 1})
	must(t, err)
	if len(list) != 1 || list[0].ID != gone.ID {
		t.Fatalf("destroyed = %+v", list)
	}
	for _, in := range []mcp.ListMachinesInput{{State: "gone"}, {Limit: -1}} {
		if _, err := f.svc.ListMachines(ctx, f.by, in); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Fatalf("%+v: %v", in, err)
		}
	}
}

type overviewFunc func(context.Context, uuid.UUID, func(uuid.UUID) bool) (fleet.Overview, error)

func (f overviewFunc) Overview(ctx context.Context, id uuid.UUID, visible func(uuid.UUID) bool) (fleet.Overview, error) {
	return f(ctx, id, visible)
}

func TestFleetOverview(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.FleetOverview(t.Context(), f.by); !errors.Is(err, mcp.ErrUnavailable) {
		t.Fatalf("no fleet: %v", err)
	}
	svc := tools.New(tools.Config{Store: f.st, Fleet: overviewFunc(func(_ context.Context, id uuid.UUID, _ func(uuid.UUID) bool) (fleet.Overview, error) {
		return fleet.Overview{Headline: id.String()}, nil
	})})
	o, err := svc.FleetOverview(t.Context(), f.by)
	must(t, err)
	if o.Headline != f.account.ID.String() {
		t.Fatalf("overview = %+v", o)
	}
}

// The fleet is the organization's: a role on one project does not read it.
func TestFleetToolsNeedWorkerRead(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	u := f.st.AddMember(f.account.ID, domain.User{Name: "someone"}, "")
	testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, u.ID, projectScope, domain.RoleProjectAdmin, f.project.ID)
	by := domain.UserActor(f.account.ID, u.ID, domain.Credential{})
	for name, call := range map[string]func() error{
		"list_machines":  func() error { _, err := f.svc.ListMachines(ctx, by, mcp.ListMachinesInput{}); return err },
		"list_shapes":    func() error { _, err := f.svc.ListShapes(ctx, by); return err },
		"list_providers": func() error { _, err := f.svc.ListProviders(ctx, by); return err },
		"fleet_overview": func() error { _, err := f.svc.FleetOverview(ctx, by); return err },
	} {
		if err := call(); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s: err = %v, want denied", name, err)
		}
	}
}

// worker.read reads the machines, and job.read decides what of their jobs
// shows, project by project.
func TestFleetKeepsJobsToTheirProjects(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	own, shape := fleetRows(t, f.st, f.account.ID, "home")
	m := domain.Machine{AccountID: f.account.ID, ProviderID: own.ID, ShapeID: shape.ID}
	must(t, f.st.Machines().Create(ctx, &m))
	j := f.newJob(t, "developer", domain.Goal{}, domain.Task{}, 0)
	_, err := f.st.Machines().TakeJob(ctx, m.ID, j.ID, f.clock.Now())
	must(t, err)

	role := domain.Role{AccountID: f.account.ID, Name: "machines", Scope: orgScope, Permissions: []domain.Permission{domain.PermWorkerRead}, Custom: true}
	must(t, f.st.Roles().Create(ctx, &role))
	u := f.st.AddMember(f.account.ID, domain.User{Name: "someone"}, "")
	testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, u.ID, orgScope, "machines", uuid.Nil())
	by := domain.UserActor(f.account.ID, u.ID, domain.Credential{})

	list, err := f.svc.ListMachines(ctx, by, mcp.ListMachinesInput{})
	must(t, err)
	if len(list) != 1 || list[0].JobRef != "" || list[0].JobID != uuid.Nil() {
		t.Fatalf("machines = %+v", list)
	}
	var seen bool
	svc := tools.New(tools.Config{Store: f.st, Fleet: overviewFunc(func(_ context.Context, _ uuid.UUID, visible func(uuid.UUID) bool) (fleet.Overview, error) {
		seen = visible(f.project.ID)
		return fleet.Overview{}, nil
	})})
	if _, err := svc.FleetOverview(ctx, by); err != nil || seen {
		t.Fatalf("err = %v, project visible = %v", err, seen)
	}

	testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, u.ID, projectScope, domain.RoleProjectAdmin, f.project.ID)
	list, err = f.svc.ListMachines(ctx, by, mcp.ListMachinesInput{})
	must(t, err)
	if list[0].JobRef != j.Ref() {
		t.Fatalf("machines = %+v", list)
	}
}
