package fleet

import (
	"cmp"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// organization is what the plan needs of one organization.
type organization struct {
	id      uuid.UUID
	policy  domain.FleetPolicy
	shapes  []domain.Shape  // its own and the instance-wide ones
	queued  []domain.Job    // oldest first
	held    []domain.Job    // queued, waiting for a model connection
	running []domain.Job    // of every worker
	workers []domain.Worker // static and ephemeral
}

// snapshot is everything a plan is made from, so that planning reads nothing
// and changes nothing: job_status and the overview run the same plan the
// controller acts on.
type snapshot struct {
	now       time.Time
	currency  string
	providers map[uuid.UUID]domain.CapacityProvider
	shapes    map[uuid.UUID]domain.Shape
	// machines are the machines that exist or ended this month.
	machines []domain.Machine
	orgs     []organization
	// jobEnds holds when the last job of an idle busy machine ended.
	jobEnds map[uuid.UUID]time.Time
}

// assignment gives a warm machine its organization.
type assignment struct {
	machine domain.Machine
	org     uuid.UUID
	expires time.Time // of its bootstrap token
}

// transition takes a machine out of service, with why. A forced one goes
// whatever it is doing; any other only if it is still not in use.
type transition struct {
	machine domain.Machine
	why     string
	forced  bool
}

// plan is what the controller should do now, and why jobs wait.
type plan struct {
	create   []domain.Machine
	assign   []assignment
	fail     []transition
	drain    []transition
	withdraw []domain.Machine
	// reasons has an entry for each job the plan looked at; the zero Reason
	// means it waits for nothing.
	reasons map[uuid.UUID]Reason
}

// usage adds up what a month cost so far (fleet.md, Fleet policy): usage
// records do not exist yet, so it reads what they will hold from the
// machines, metered from assigned_at to destroyed_at or now.
type usage struct {
	shapeCost    map[uuid.UUID]float64
	providerCost map[uuid.UUID]float64
	orgMinutes   map[uuid.UUID]float64
	orgCost      map[uuid.UUID]float64 // every machine
	orgHosted    map[uuid.UUID]float64 // hosted machines only
}

// monthStart returns the first moment of t's calendar month in UTC.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (s *snapshot) usage() usage {
	u := usage{map[uuid.UUID]float64{}, map[uuid.UUID]float64{}, map[uuid.UUID]float64{}, map[uuid.UUID]float64{}, map[uuid.UUID]float64{}}
	start := monthStart(s.now)
	for _, m := range s.machines {
		shape, ok := s.shapes[m.ShapeID]
		if !ok || m.AssignedAt.IsZero() {
			continue
		}
		end := m.DestroyedAt
		if end.IsZero() {
			end = s.now
		}
		from := m.AssignedAt
		if from.Before(start) {
			from = start
		}
		minutes := end.Sub(from).Minutes()
		if minutes <= 0 {
			continue
		}
		cost := minutes * shape.PricePerMinute
		u.shapeCost[m.ShapeID] += cost
		u.providerCost[m.ProviderID] += cost
		u.orgMinutes[m.AccountID] += minutes
		u.orgCost[m.AccountID] += cost
		if shape.Ownership() == domain.OwnershipHosted {
			u.orgHosted[m.AccountID] += cost
		}
	}
	return u
}

// planner keeps the counts a plan changes as it goes, so one pass cannot
// exceed a limit however many jobs wait.
type planner struct {
	s     *snapshot
	out   *plan
	usage usage
	orgs  map[uuid.UUID]*organization

	shapeCount    map[uuid.UUID]int
	providerCount map[uuid.UUID]int
	orgCount      map[uuid.UUID]int
	// runningOn counts running jobs by worker.
	runningOn map[uuid.UUID]int
	// live holds the machines that can still take jobs, the most ready
	// first, and slots how many more each can take.
	live    []*domain.Machine
	slots   map[uuid.UUID]int
	claimed map[uuid.UUID]bool
	// inLine counts the jobs waiting for a shape, or for a pool's workers.
	inLine map[string]int
}

func (s *snapshot) plan() *plan {
	p := &planner{
		s: s, out: &plan{reasons: map[uuid.UUID]Reason{}}, usage: s.usage(), orgs: map[uuid.UUID]*organization{},
		shapeCount: map[uuid.UUID]int{}, providerCount: map[uuid.UUID]int{}, orgCount: map[uuid.UUID]int{},
		runningOn: map[uuid.UUID]int{}, slots: map[uuid.UUID]int{}, claimed: map[uuid.UUID]bool{}, inLine: map[string]int{},
	}
	for i := range s.orgs {
		o := &s.orgs[i]
		p.orgs[o.id] = o
		for _, j := range o.running {
			p.runningOn[j.WorkerID]++
		}
	}
	p.sweep()
	for i := range s.orgs {
		p.placeAll(&s.orgs[i])
	}
	p.keepWarm()
	p.trim()
	return p.out
}

func (p *planner) count(m domain.Machine, by int) {
	p.shapeCount[m.ShapeID] += by
	p.providerCount[m.ProviderID] += by
	if m.AccountID != uuid.Nil() {
		p.orgCount[m.AccountID] += by
	}
}

// bootExpired reports a machine that used up its boot timeout: not up in
// time, or up and then assigned, with no worker exchanging its token. A warm
// machine is up and stays until demand comes, so its time starts again when
// it is assigned.
func bootExpired(m domain.Machine, s domain.Shape, now time.Time) bool {
	if m.BootExpired(s, now) {
		return true
	}
	if m.State != domain.MachineBooting || m.AccountID == uuid.Nil() || m.ReadyAt.IsZero() {
		return false
	}
	start := m.AssignedAt
	if m.BootingAt.After(start) {
		start = m.BootingAt
	}
	return !now.Before(start.Add(time.Duration(s.BootTimeoutSeconds) * time.Second))
}

// sweep takes finished and broken machines out and lists those left.
func (p *planner) sweep() {
	for i := range p.s.machines {
		m := &p.s.machines[i]
		shape, ok := p.s.shapes[m.ShapeID]
		if !ok || !m.State.Counts() {
			continue
		}
		p.count(*m, 1)
		pr := p.s.providers[m.ProviderID]
		switch {
		case pr.Status == domain.ProviderRevoked && pr.Mode == domain.ProviderModeDirect:
			p.revoked(m)
		case pr.Status == domain.ProviderRevoked:
			p.cutOff(m)
		case bootExpired(*m, shape, p.s.now):
			p.out.fail = append(p.out.fail, transition{machine: *m, why: fmt.Sprintf("did not come up within %d seconds", shape.BootTimeoutSeconds)})
			p.count(*m, -1)
		case m.State == domain.MachineDraining:
		case !m.DeadlineAt.IsZero() && !p.s.now.Before(m.DeadlineAt):
			p.out.drain = append(p.out.drain, transition{*m, "past its deadline", true})
		case m.State == domain.MachineBusy && p.runningOn[m.WorkerID] == 0 && m.JobsTaken >= shape.MaxJobs:
			p.out.drain = append(p.out.drain, transition{machine: *m, why: "finished its jobs"})
		case m.JobsTaken < shape.MaxJobs:
			p.slots[m.ID] = shape.MaxJobs - m.JobsTaken
			p.live = append(p.live, m)
		}
	}
	slices.SortStableFunc(p.live, func(a, b *domain.Machine) int {
		if c := cmp.Compare(readiness(a.State), readiness(b.State)); c != 0 {
			return c
		}
		return cmp.Compare(b.ReadyAt.Unix(), a.ReadyAt.Unix()) // warm before still booting
	})
}

// revoked ends a machine of a revoked direct provider, whatever it is doing.
func (p *planner) revoked(m *domain.Machine) {
	switch m.State {
	case domain.MachineDraining:
	case domain.MachineRequested:
		p.out.withdraw = append(p.out.withdraw, *m)
	default:
		p.out.drain = append(p.out.drain, transition{*m, "its location was revoked", true})
	}
}

// cutOff ends the row of a machine of a revoked agent provider: the agent lost
// its token, so nothing will ever start this machine or say it is gone. It
// must not hold a limit for good, and its job is requeued through its lease.
func (p *planner) cutOff(m *domain.Machine) {
	p.count(*m, -1)
	if m.State == domain.MachineRequested {
		p.out.withdraw = append(p.out.withdraw, *m)
		return
	}
	p.out.fail = append(p.out.fail, transition{machine: *m, why: "its location was revoked"})
}

func readiness(s domain.MachineState) int {
	return slices.Index([]domain.MachineState{domain.MachineReady, domain.MachineBusy, domain.MachineBooting, domain.MachineRequested}, s)
}

// placeAll finds somewhere to run each queued job, oldest first.
func (p *planner) placeAll(o *organization) {
	free := map[uuid.UUID]int{}
	for _, w := range o.workers {
		if !w.Ephemeral && w.Online(p.s.now) {
			free[w.ID] = max(w.MaxJobs-p.runningOn[w.ID], 0)
		}
	}
	for _, j := range o.queued {
		if j.Pool == domain.PoolActions || j.RunAfter.After(p.s.now) {
			continue // the Actions provider's, or held back by its retry
		}
		p.out.reasons[j.ID] = p.place(o, j, free)
	}
}

// place runs through the options of one job: an idle static worker, a machine
// of its organization, a warm machine, a new machine. No machine is created
// for a job a static worker can lease (fleet.md).
func (p *planner) place(o *organization, j domain.Job, free map[uuid.UUID]int) Reason {
	d := j.ShapeDemand()
	for _, w := range o.workers {
		if free[w.ID] > 0 && w.Pool == d.Pool && domain.MatchLabels(d.Labels, w.Labels) {
			free[w.ID]--
			return Reason{}
		}
	}
	if m := p.claim(o.id, d); m != nil {
		return p.claimedReason(*m)
	}
	matches := domain.SelectShapes(o.shapes, o.policy, d)
	if m := p.assignWarm(o, matches); m != nil {
		return p.claimedReason(*m)
	}
	var blocks []*block
	for _, s := range matches {
		b := p.room(o.id, s, true)
		if b == nil {
			p.create(o.id, s)
			return bootingReason(p.bootEstimate(s.ID))
		}
		blocks = append(blocks, b)
	}
	return p.why(o, j, matches, blocks)
}

// claim reserves a place on a machine of the organization. Its shape need
// not be enabled or allowed any more: the machine exists and leases by itself.
func (p *planner) claim(org uuid.UUID, d domain.ShapeDemand) *domain.Machine {
	for _, m := range p.live {
		if m.AccountID != org || p.slots[m.ID] == 0 {
			continue
		}
		shape := p.s.shapes[m.ShapeID]
		shape.Enabled = true
		if shape.Serves(d) {
			p.take(m)
			return m
		}
	}
	return nil
}

func (p *planner) take(m *domain.Machine) {
	p.slots[m.ID]--
	p.claimed[m.ID] = true
}

// assignWarm gives the organization the unassigned machine of the best shape
// in matches. An assignment starts metering, so limits of money apply.
func (p *planner) assignWarm(o *organization, matches []domain.Shape) *domain.Machine {
	for _, s := range matches {
		for _, m := range p.live {
			if m.ShapeID != s.ID || m.AccountID != uuid.Nil() || p.room(o.id, s, false) != nil {
				continue
			}
			p.out.assign = append(p.out.assign, assignment{*m, o.id, p.s.now.Add(time.Duration(s.BootTimeoutSeconds) * time.Second)})
			m.AccountID, m.AssignedAt = o.id, p.s.now
			p.orgCount[o.id]++
			p.take(m)
			return m
		}
	}
	return nil
}

func (p *planner) create(org uuid.UUID, s domain.Shape) {
	m := domain.Machine{ID: domain.NewID(), AccountID: org, ProviderID: s.ProviderID, ShapeID: s.ID, State: domain.MachineRequested}
	p.out.create = append(p.out.create, m)
	p.count(m, 1)
}

// claimedReason says what a job on machine m waits for.
func (p *planner) claimedReason(m domain.Machine) Reason {
	switch m.State {
	case domain.MachineReady, domain.MachineBusy:
		return Reason{}
	case domain.MachineRequested:
		if pr := p.s.providers[m.ProviderID]; !pr.CanStart(p.s.now) {
			return offlineReason([]string{displayName(pr.Name)}, providerState(pr))
		}
	}
	if m.Warm() || !m.ReadyAt.IsZero() {
		return Reason{Code: ReasonBooting, Sentence: "Starting a machine"}
	}
	return bootingReason(p.bootEstimate(m.ShapeID))
}

// bootEstimate is how long machines of the shape took to come up this month.
func (p *planner) bootEstimate(shape uuid.UUID) time.Duration {
	var total time.Duration
	n := 0
	for _, m := range p.s.machines {
		if m.ShapeID == shape && !m.BootingAt.IsZero() && m.ReadyAt.After(m.BootingAt) {
			total += m.ReadyAt.Sub(m.BootingAt)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return total / time.Duration(n)
}

func providerState(pr domain.CapacityProvider) string {
	if pr.Status == domain.ProviderPaused {
		return "paused"
	}
	return "offline"
}

// block is what stops a shape from taking a machine now.
type block struct {
	reason Reason
	// offline names the location that cannot start it, and status says
	// how; empty is its own state.
	offline domain.CapacityProvider
	status  string
	queue   bool
}

// room returns what stops a machine of s for org, nil when nothing does. org
// zero is a warm machine, which no organization pays for yet. isNew says it
// would be one more machine, not one already up and only being assigned.
func (p *planner) room(org uuid.UUID, s domain.Shape, isNew bool) *block {
	pr := p.s.providers[s.ProviderID]
	cur := p.s.currency
	if !pr.CanStart(p.s.now) {
		return &block{offline: pr}
	}
	if isNew && p.failing(s) {
		return &block{offline: pr, status: "failing to start machines"}
	}
	if isNew && p.shapeCount[s.ID] >= s.Max {
		return &block{queue: true}
	}
	if n := pr.MaxMachines(); isNew && n > 0 && p.providerCount[pr.ID] >= n {
		return limited(displayName(pr.Name) + " is at its limit of " + plural(n, "machine") + " at once")
	}
	if b := s.MonthlyBudget; b != nil && p.usage.shapeCost[s.ID] >= *b {
		return spent(displayName(s.Name) + "'s limit of " + money(cur, *b) + " this month is used up")
	}
	if b := pr.Limits.MonthlyBudget; b != nil && p.usage.providerCost[pr.ID] >= *b {
		return spent(displayName(pr.Name) + "'s limit of " + money(cur, *b) + " this month is used up")
	}
	o := p.orgs[org]
	if o == nil {
		return nil
	}
	q := o.policy.Quotas
	if n := q.ConcurrentMachines; n > 0 && p.orgCount[org] >= n {
		return limited("At your limit of " + plural(n, "machine") + " at once")
	}
	if n := q.MonthlyMinutes; n > 0 && p.usage.orgMinutes[org] >= float64(n) {
		return spent("This month's limit of " + plural(n, "minute") + " is used up")
	}
	if b := q.MonthlyBudget; b != nil && s.Ownership() == domain.OwnershipHosted && p.usage.orgHosted[org] >= *b {
		return spent("This month's limit of " + money(cur, *b) + " is used up")
	}
	return nil
}

// failing reports a shape that should wait before its next machine: the
// first failure is replaced at once, and from the second on the wait doubles
// as a job's retry does. It reads the machines, so a restart keeps it: each
// failure left a row. A machine that came up ends the count.
func (p *planner) failing(s domain.Shape) bool {
	failures := 0
	var last time.Time
	for _, m := range slices.Backward(p.s.machines) {
		if m.ShapeID != s.ID {
			continue
		}
		if !m.ReadyAt.IsZero() {
			break
		}
		if m.Error != "" {
			failures++
			if m.UpdatedAt.After(last) {
				last = m.UpdatedAt
			}
		}
	}
	return failures >= 2 && p.s.now.Before(last.Add(domain.RetryBackoff(failures-1)))
}

func limited(sentence string) *block {
	return &block{reason: Reason{Code: ReasonLimit, Sentence: sentence}}
}

func spent(sentence string) *block {
	return &block{reason: Reason{Code: ReasonBudget, Sentence: sentence}}
}

// why explains a job nothing could take. A limit on a location that is up
// says more than another location being offline, so offline comes last.
func (p *planner) why(o *organization, j domain.Job, matches []domain.Shape, blocks []*block) Reason {
	d := j.ShapeDemand()
	offline := []string{}
	status := "offline"
	for i, b := range blocks {
		switch {
		case b.queue:
			key := matches[i].ID.String()
			p.inLine[key]++
			return queueReason(p.inLine[key], displayName(matches[i].Name))
		case b.reason != Reason{}:
			return b.reason
		}
		offline = append(offline, displayName(b.offline.Name))
		status = cmp.Or(b.status, providerState(b.offline))
	}
	if len(offline) > 0 {
		return offlineReason(offline, status)
	}
	return p.noMatch(o, d)
}

// noMatch explains a job no shape of the organization's policy serves.
func (p *planner) noMatch(o *organization, d domain.ShapeDemand) Reason {
	online := false
	for _, w := range o.workers {
		if w.Ephemeral || !w.Online(p.s.now) || w.Pool != d.Pool {
			continue
		}
		online = true
		if domain.MatchLabels(d.Labels, w.Labels) {
			p.inLine[d.Pool]++
			return queueReason(p.inLine[d.Pool], "a free machine")
		}
	}
	open := o.policy
	open.Routing, open.AllowedShapes = domain.RoutingOwnFirst, nil
	if other := domain.SelectShapes(o.shapes, open, d); len(other) > 0 {
		return policyReason(displayName(p.s.providers[other[0].ProviderID].Name), o.policy.Routing)
	}
	inPool := slices.ContainsFunc(o.shapes, func(s domain.Shape) bool { return s.Enabled && domain.OrDefaultPool(s.Pool) == d.Pool })
	if d.Pool != domain.PoolBuild && !online && !inPool {
		return zoneReason(d.Pool)
	}
	return noShapeReason(d)
}

// waitsForJob reports a machine with no job so far.
func waitsForJob(m *domain.Machine) bool {
	switch m.State {
	case domain.MachineRequested, domain.MachineBooting:
		return true
	case domain.MachineReady:
		return m.JobID == uuid.Nil()
	}
	return false
}

// keepWarm starts machines until every enabled shape has its minimum up with
// no job. Machines on their way count, or every pass would add more.
func (p *planner) keepWarm() {
	have := map[uuid.UUID]int{}
	for _, m := range p.live {
		if !p.claimed[m.ID] && waitsForJob(m) {
			have[m.ShapeID]++
		}
	}
	shapes := make([]domain.Shape, 0, len(p.s.shapes))
	for _, s := range p.s.shapes {
		shapes = append(shapes, s)
	}
	slices.SortFunc(shapes, func(a, b domain.Shape) int { return a.ID.Compare(b.ID) })
	for _, s := range shapes {
		for s.Enabled && have[s.ID] < s.WarmMin && p.room(s.AccountID, s, true) == nil {
			p.create(s.AccountID, s)
			have[s.ID]++
		}
	}
}

// idleSince returns when machine m last had something to do.
func (p *planner) idleSince(m *domain.Machine) time.Time {
	t := m.ReadyAt
	for _, u := range []time.Time{m.AssignedAt, p.s.jobEnds[m.ID]} {
		if u.After(t) {
			t = u
		}
	}
	return t
}

// trim ends what no job needs and warm_min does not keep: a machine not
// started yet at once, one that is up when its idle time is over. A machine
// still booting comes up first; ready machines are kept before those.
func (p *planner) trim() {
	kept := map[uuid.UUID]int{}
	for _, m := range p.live {
		shape := p.s.shapes[m.ShapeID]
		if p.claimed[m.ID] || p.runningOn[m.WorkerID] > 0 {
			continue
		}
		if shape.Enabled && waitsForJob(m) && kept[m.ShapeID] < shape.WarmMin {
			kept[m.ShapeID]++
			continue
		}
		switch {
		case m.State == domain.MachineRequested:
			p.out.withdraw = append(p.out.withdraw, *m)
		case m.State == domain.MachineBooting && !m.Warm():
		case !p.s.now.Before(p.idleSince(m).Add(time.Duration(shape.IdleSeconds) * time.Second)):
			p.out.drain = append(p.out.drain, transition{machine: *m, why: "idle"})
		}
	}
}
