package aws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetapi"
	"github.com/forecrew/forecrew/internal/store"
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
	storeClock := &clock{}
	st := testkit.NewStore(storeClock.now)
	account := domain.Account{Name: "acme"}
	must(t, st.Accounts().Create(ctx, &account))
	token, hash := fleet.NewProviderToken()
	provider := domain.CapacityProvider{AccountID: account.ID, Name: "own-aws", Kind: domain.ProviderAWS, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, TokenHash: hash}
	must(t, st.CapacityProviders().Create(ctx, &provider))
	shape := domain.Shape{
		AccountID: account.ID, ProviderID: provider.ID, Name: "aws-spot", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 30,
		Image: "linux", Max: 5, Enabled: true, Labels: []string{"runtime:codex"}, ProviderConfig: map[string]any{"instance_type": "c7i.large", "spot": true},
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
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

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

	// The guest: it reads its token from the user data and trades it for a worker,
	// which leases a job.
	worker, job := bootAndLease(t, e, st, account.ID, live[0], m.ID)

	// EC2 takes the spot instance back, a while after it started.
	e.clock.advance(3 * consistencyWindow)
	e.ec2.interrupt(live[0])
	eventually(t, "forecrewd to fail the lost machine", func() bool { return state().Error != "" })
	// The agent then reports the failed machine destroyed, which is what ends it.
	if got := state(); got.Error != "gone from the provider" || (got.State != domain.MachineFailed && got.State != domain.MachineDestroyed) {
		t.Fatalf("machine = %+v", got)
	}
	// Its worker is revoked, and when the lease runs out the job is queued again.
	w, err := st.Workers().Get(ctx, account.ID, worker.ID)
	must(t, err)
	if !w.Revoked() {
		t.Fatal("the worker of a lost machine still works")
	}
	storeClock.advance(domain.LeaseDuration + time.Hour + time.Minute)
	requeued, err := st.Jobs().RequeueExpired(ctx, account.ID)
	must(t, err)
	if len(requeued) != 1 || requeued[0].ID != job.ID {
		t.Fatalf("requeued = %+v, want job %s", requeued, job.ID)
	}
	if got, err := st.Jobs().Get(ctx, account.ID, job.ID); err != nil || got.Status != domain.JobQueued {
		t.Fatalf("job = %+v, %v", got, err)
	}
}

// bootAndLease plays the machine's guest: the bootstrap token in the instance's
// user data is traded for a worker, which leases a job of its organization.
func bootAndLease(t *testing.T, e *env, st *testkit.Store, account uuid.UUID, instance string, machine uuid.UUID) (domain.Worker, domain.Job) {
	t.Helper()
	ctx := t.Context()
	e.ec2.mu.Lock()
	raw, err := base64.StdEncoding.DecodeString(e.ec2.userData[instance])
	e.ec2.mu.Unlock()
	must(t, err)
	var token string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "FORECREW_BOOTSTRAP_TOKEN="); ok {
			token = v
		}
	}
	sum := sha256.Sum256([]byte(token))
	grant, err := fleet.Exchange(ctx, st, sum[:], "v1")
	must(t, err)
	if grant.Wait {
		t.Fatal("told to wait")
	}
	m, err := st.Machines().Get(ctx, machine)
	must(t, err)
	w, err := st.Workers().Get(ctx, account, m.WorkerID)
	must(t, err)
	must(t, st.Workers().Seen(ctx, account, w.ID, w.Labels, "v1"))
	project := domain.Project{AccountID: account, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &project))
	job := domain.Job{AccountID: account, ProjectID: project.ID, Role: "developer", Runtime: "codex", Spec: domain.JobSpec{Repo: "acme/app", Prompt: "go", Timeout: domain.Duration(time.Hour)}}
	must(t, st.Jobs().Create(ctx, &job))
	var leased domain.Job
	must(t, st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if leased, err = tx.Jobs().LeaseNext(ctx, account, w.ID, w.Labels, domain.LeaseDuration); err != nil {
			return err
		}
		return fleet.RecordLease(ctx, tx, w, leased)
	}))
	if leased.ID != job.ID {
		t.Fatalf("leased %s, want %s", leased.ID, job.ID)
	}
	return w, leased
}
