package fleet

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Explain says why queued job j waits. ok is false for a job the fleet has no
// say in: of pool actions, or held back by its retry.
func (c *Controller) Explain(ctx context.Context, j domain.Job) (r Reason, ok bool, err error) {
	s, err := c.load(ctx, j.AccountID)
	if err != nil {
		return Reason{}, false, err
	}
	r, ok = s.plan().reasons[j.ID]
	return r, ok, nil
}

// WaitReason is Explain in plain strings, for jobs.Waiting.
func (c *Controller) WaitReason(ctx context.Context, j domain.Job) (code, sentence string, ok bool, err error) {
	r, ok, err := c.Explain(ctx, j)
	return string(r.Code), r.Sentence, ok, err
}

// Location kinds of the overview.
const (
	LocationProvider = "location" // a capacity provider
	LocationComputer = "computer" // a static worker: runs jobs directly
)

// Location statuses of the overview.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
	StatusPaused  = "paused"
	StatusPending = "pending"
	StatusRevoked = "revoked"
)

// Location is one row of the overview: a capacity provider or a static
// worker, so people see everywhere jobs can run in one list.
type Location struct {
	ID     uuid.UUID `json:"id"`
	Kind   string    `json:"kind"`
	Name   string    `json:"name"`
	Runs   string    `json:"runs"`   // Linux, Windows
	Status string    `json:"status"` // Status*
	InUse  int       `json:"in_use"`
	// Capacity is how many it can run at once; 0 means no limit.
	Capacity int    `json:"capacity"`
	Cost     string `json:"cost"` // Free, or the lowest price
}

// WaitingJob is a queued job with why it waits.
type WaitingJob struct {
	Ref      string     `json:"ref"`
	Role     string     `json:"role,omitempty"`
	Code     ReasonCode `json:"code"`
	Sentence string     `json:"sentence"`
}

// RunningJob is a running job with where it runs.
type RunningJob struct {
	Ref      string `json:"ref"`
	Role     string `json:"role,omitempty"`
	Machine  string `json:"machine"`
	Location string `json:"location"`
	Minutes  int    `json:"minutes"`
}

// Spend is what this month's machines cost so far. Limit is 0 without one.
type Spend struct {
	Amount   float64 `json:"amount"`
	Limit    float64 `json:"limit,omitempty"`
	Currency string  `json:"currency"`
}

// Overview is the Machines page: whether jobs have somewhere to run, and
// where. Sentences are written here, so every client shows the same ones.
type Overview struct {
	Headline  string              `json:"headline"`
	Working   int                 `json:"working"`
	Waiting   int                 `json:"waiting"`
	Spend     *Spend              `json:"spend,omitempty"` // nil while everything is free
	Routing   domain.FleetRouting `json:"routing"`
	Locations []Location          `json:"locations"`
	Queue     []WaitingJob        `json:"queue"`
	Running   []RunningJob        `json:"running"`
}

// Overview returns the overview of an organization. Machines and locations
// are the organization's; of its jobs it lists those of the projects visible
// says the caller may read.
func (c *Controller) Overview(ctx context.Context, accountID uuid.UUID, visible func(project uuid.UUID) bool) (Overview, error) {
	s, err := c.load(ctx, accountID)
	if err != nil {
		return Overview{}, err
	}
	if len(s.orgs) == 0 {
		return Overview{}, fmt.Errorf("organization %s: %w", accountID, store.ErrNotFound)
	}
	o := &s.orgs[0]
	p := s.plan()
	out := Overview{Routing: o.policy.Routing, Locations: s.locations(o), Queue: []WaitingJob{}, Running: s.runningJobs(o, visible)}
	out.Working = len(out.Running)
	for _, j := range o.queued {
		if r := p.reasons[j.ID]; r != (Reason{}) && visible(j.ProjectID) {
			out.Queue = append(out.Queue, WaitingJob{Ref: j.Ref(), Role: j.Role, Code: r.Code, Sentence: r.Sentence})
		}
	}
	for _, j := range o.held {
		if !visible(j.ProjectID) || c.cfg.Models == nil {
			continue
		}
		code, sentence, ok, err := c.cfg.Models.WaitReason(ctx, j)
		if err != nil {
			return Overview{}, err
		}
		if ok {
			out.Queue = append(out.Queue, WaitingJob{Ref: j.Ref(), Role: j.Role, Code: ReasonCode(code), Sentence: sentence})
		}
	}
	out.Waiting = len(out.Queue)
	u := s.usage()
	if amount := u.orgCost[o.id]; amount > 0 {
		out.Spend = &Spend{Amount: amount, Currency: s.currency}
		if b := o.policy.Quotas.MonthlyBudget; b != nil {
			out.Spend.Limit = *b
		}
	}
	out.Headline = headline(out)
	return out, nil
}

// headline is one sentence with the worst thing: what only a person can fix
// before what fixes itself.
func headline(o Overview) string {
	if len(o.Queue) > 0 {
		i := slices.IndexFunc(o.Queue, func(w WaitingJob) bool { return w.Code != ReasonBooting && w.Code != ReasonQueue })
		return plural(len(o.Queue), "job") + " waiting: " + o.Queue[max(i, 0)].Sentence + "."
	}
	if len(o.Locations) == 0 {
		return "No machines yet. Agents need somewhere to work."
	}
	return "Everything is fine. " + plural(o.Working, "job") + " running, none waiting."
}

// locations lists the organization's providers, the instance-wide ones its
// policy lets it use, and its static workers.
func (s *snapshot) locations(o *organization) []Location {
	var out []Location
	shapesOf := map[uuid.UUID][]domain.Shape{}
	for _, sh := range o.shapes {
		if sh.Enabled && o.policy.Allows(sh) {
			shapesOf[sh.ProviderID] = append(shapesOf[sh.ProviderID], sh)
		}
	}
	inUse := map[uuid.UUID]int{}
	for _, m := range s.machines {
		if m.State.Counts() && m.AccountID == o.id {
			inUse[m.ProviderID]++
		}
	}
	for _, pr := range s.providers {
		if pr.AccountID != o.id && len(shapesOf[pr.ID]) == 0 {
			continue
		}
		out = append(out, Location{
			ID: pr.ID, Kind: LocationProvider, Name: displayName(pr.Name), Runs: runs(shapesOf[pr.ID]), Status: s.status(pr),
			InUse: inUse[pr.ID], Capacity: pr.MaxMachines(), Cost: s.cost(shapesOf[pr.ID]),
		})
	}
	running := map[uuid.UUID]int{}
	for _, j := range o.running {
		running[j.WorkerID]++
	}
	for _, w := range o.workers {
		if w.Ephemeral || w.Revoked() || w.Pool == domain.PoolActions {
			continue
		}
		status := StatusOffline
		if w.Online(s.now) {
			status = StatusOnline
		}
		out = append(out, Location{ID: w.ID, Kind: LocationComputer, Name: w.Name, Runs: workerRuns(w), Status: status, InUse: running[w.ID], Capacity: w.MaxJobs, Cost: "Free"})
	}
	slices.SortFunc(out, func(a, b Location) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

func (s *snapshot) status(pr domain.CapacityProvider) string {
	switch {
	case pr.Status == domain.ProviderRevoked:
		return StatusRevoked
	case pr.Status == domain.ProviderPending:
		return StatusPending
	case pr.Status == domain.ProviderPaused:
		return StatusPaused
	case pr.Online(s.now):
		return StatusOnline
	}
	return StatusOffline
}

// runs lists the operating systems of shapes.
func runs(shapes []domain.Shape) string {
	var names []string
	for _, sh := range shapes {
		names = append(names, osName(string(sh.OS)))
	}
	slices.Sort(names)
	return strings.Join(slices.Compact(names), ", ")
}

func osName(os string) string {
	switch os {
	case "windows":
		return "Windows"
	case "macos", "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	}
	return ""
}

// workerRuns says what a static worker runs, directly.
func workerRuns(w domain.Worker) string {
	for _, l := range w.Labels {
		if name := osName(l); name != "" {
			return name + ", directly"
		}
	}
	return "Directly"
}

// cost is the lowest price of shapes.
func (s *snapshot) cost(shapes []domain.Shape) string {
	lowest := 0.0
	for _, sh := range shapes {
		if p := sh.PricePerMinute; p > 0 && (lowest == 0 || p < lowest) {
			lowest = p
		}
	}
	if lowest == 0 {
		return "Free"
	}
	return "~" + money(s.currency, lowest) + "/min"
}

// runningJobs lists what runs now, oldest first.
func (s *snapshot) runningJobs(o *organization, visible func(uuid.UUID) bool) []RunningJob {
	onWorker := map[uuid.UUID]domain.Machine{}
	for _, m := range s.machines {
		if m.State.Counts() && m.WorkerID != uuid.Nil() {
			onWorker[m.WorkerID] = m
		}
	}
	names := map[uuid.UUID]string{}
	for _, w := range o.workers {
		names[w.ID] = w.Name
	}
	out := []RunningJob{}
	for _, j := range slices.Backward(o.running) {
		if !visible(j.ProjectID) {
			continue
		}
		r := RunningJob{Ref: j.Ref(), Role: j.Role, Machine: names[j.WorkerID], Location: names[j.WorkerID], Minutes: int(s.now.Sub(j.StartedAt) / time.Minute)}
		if m, ok := onWorker[j.WorkerID]; ok {
			r.Machine = MachineName(s.shapes[m.ShapeID], m)
			r.Location = displayName(s.providers[m.ProviderID].Name)
		}
		out = append(out, r)
	}
	return out
}

// MachineName is what people call a machine: linux-small-8f2.
func MachineName(s domain.Shape, m domain.Machine) string {
	return s.Name + "-" + idTail(m.ID, 3)
}
