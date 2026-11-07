package aws

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetapi"
	"github.com/forecrew/forecrew/internal/testkit"
)

// The agent, the AWS provider on a fake EC2 and forecrewd's fleet API on a fake
// store, all together: a machine is requested, started in the account, taken back
// by EC2 as a spot instance is, and forecrewd fails it.
func TestAgentStartsAndLosesASpotMachine(t *testing.T) {
	e := newEnv(t, nil)
	defer func() {
		if t.Failed() {
			t.Log(e.logs.String())
		}
	}()
	ctx := t.Context()
	st := testkit.NewStore(func() time.Time { return time.Now().UTC() })
	account := domain.Account{Name: "acme"}
	must(t, st.Accounts().Create(ctx, &account))
	token, hash := fleet.NewProviderToken()
	provider := domain.CapacityProvider{AccountID: account.ID, Name: "own-aws", Kind: domain.ProviderAWS, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, TokenHash: hash}
	must(t, st.CapacityProviders().Create(ctx, &provider))
	shape := domain.Shape{
		AccountID: account.ID, ProviderID: provider.ID, Name: "aws-spot", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 30,
		Image: "linux", Max: 5, Enabled: true, ProviderConfig: map[string]any{"instance_type": "c7i.large", "spot": true},
	}
	must(t, st.Shapes().Create(ctx, &shape))

	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := fleet.New(fleet.Config{
		Store: st, Logger: log, ServerURL: "https://forecrew.example.com",
		Image: func(context.Context, domain.CapacityProvider, domain.Shape, uuid.UUID) (domain.Image, error) {
			return e.img, nil
		},
	})
	api := fleetapi.New(fleetapi.Config{Store: st, Fleet: c, Logger: log, PollWait: 30 * time.Millisecond, PollRecheck: 3 * time.Millisecond}).Handler()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	agent := fleetagent.New(fleetagent.Config{
		Client: fleetagent.NewClient(srv.URL, token, nil), Provider: e.p, Version: "test", Logger: log,
		ReconcileEvery: 5 * time.Millisecond, BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond, NoRoomWait: 20 * time.Millisecond,
	})
	go func() { done <- agent.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })

	m := domain.Machine{AccountID: account.ID, ProviderID: provider.ID, ShapeID: shape.ID}
	must(t, st.Machines().Create(ctx, &m))
	state := func() domain.Machine {
		got, err := st.Machines().Get(ctx, m.ID)
		must(t, err)
		return got
	}
	eventually(t, "the machine to boot", func() bool { return state().State == domain.MachineBooting })
	live := e.ec2.live()
	if len(live) != 1 || state().ExternalID != live[0] {
		t.Fatalf("live = %v, machine = %+v", live, state())
	}
	if e.ec2.tag(live[0], fleet.TagMachine) != m.ID.String() || e.ec2.tag(live[0], fleet.TagOrg) != account.ID.String() {
		t.Fatalf("tags of the instance: %+v", e.ec2.get(live[0]).Tags)
	}

	// EC2 takes the spot instance back, a while after it started.
	e.clock.advance(3 * consistencyWindow)
	e.ec2.interrupt(live[0])
	eventually(t, "forecrewd to fail the lost machine", func() bool { return state().Error != "" })
	// The agent then reports the failed machine destroyed, which is what ends it.
	if got := state(); got.Error != "gone from the provider" || (got.State != domain.MachineFailed && got.State != domain.MachineDestroyed) {
		t.Fatalf("machine = %+v", got)
	}
}
