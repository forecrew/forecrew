package testkit

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

var _ store.Store = (*Store)(nil)

// Store is an in-memory store.Store. Transactions are serialized and roll
// back on error; calls outside a transaction wait for running ones.
type Store struct {
	shared *storeShared
	inTx   bool
}

type storeShared struct {
	txMu sync.Mutex // held by a running transaction
	mu   sync.Mutex // guards d
	now  func() time.Time
	d    *storeData
}

type counterKey struct {
	account uuid.UUID
	kind    domain.RefKind
}

type storeData struct {
	accounts      table[domain.Account]
	projects      table[domain.Project]
	goals         table[domain.Goal]
	specs         table[domain.Spec]
	tasks         table[domain.Task]
	changes       table[domain.Change]
	jobs          table[domain.Job]
	jobEvents     table[domain.JobEvent]
	gates         table[domain.Gate]
	signals       table[domain.Signal]
	workers       table[domain.Worker]
	runs          table[domain.WorkflowRun]
	steps         table[domain.StepRun]
	tokens        table[domain.MCPToken]
	audit         table[domain.AuditEntry]
	artifacts     table[domain.Artifact]
	testRuns      table[domain.TestRun]
	testResult    table[domain.TestResult]
	verifications table[domain.VerificationReport]
	assumptions   table[domain.Assumption]
	decisions     table[domain.Decision]
	councils      table[domain.Council]
	proposals     table[domain.Proposal]
	council       table[domain.CouncilContribution] // contributions
	deliveries    map[string]domain.WebhookDelivery
	counters      map[counterKey]int64
	knowledge     table[domain.KnowledgeEntry]
	kbProposals   table[domain.KnowledgeProposal]
	embeddings    map[uuid.UUID][]float32
	identity      identityData
	access        accessData
	providers     table[domain.CapacityProvider]
	shapes        table[domain.Shape]
	machines      table[domain.Machine]
	bootstrap     table[domain.BootstrapToken]
	joinCodes     table[domain.ProviderJoinCode]
	policies      table[domain.FleetPolicy]
	trackers      trackerData
	images        table[domain.Image]
	configPlans   table[domain.ConfigPlan]
	settings      table[domain.ConfigSetting]
	secrets       table[domain.StoredSecret]
	modelConns    table[domain.ModelConnection]
	modelAssigns  table[domain.ModelAssignment]
	modelAuths    table[domain.ModelAuthorization]
	conversations table[domain.AssistantConversation]
	messages      table[domain.AssistantMessage]
	// jobMCP holds the job MCP token hash of a job with the lease it was
	// set for; a hash whose lease ended no longer matches (the Postgres
	// trigger clears it).
	jobMCP map[uuid.UUID]jobMCPHash
}

type jobMCPHash struct {
	hash     []byte
	worker   uuid.UUID
	attempts int
}

// NewStore returns an empty store. now defaults to the real UTC time.
func NewStore(now func() time.Time) *Store {
	return &Store{shared: &storeShared{now: orNow(now), d: &storeData{
		accounts:      table[domain.Account]{key: func(a domain.Account) (uuid.UUID, uuid.UUID) { return a.ID, a.ID }},
		projects:      table[domain.Project]{key: func(r domain.Project) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		goals:         table[domain.Goal]{key: func(r domain.Goal) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		specs:         table[domain.Spec]{key: func(r domain.Spec) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		tasks:         table[domain.Task]{key: func(r domain.Task) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		changes:       table[domain.Change]{key: func(r domain.Change) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		jobs:          table[domain.Job]{key: func(r domain.Job) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		jobEvents:     table[domain.JobEvent]{key: func(r domain.JobEvent) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		gates:         table[domain.Gate]{key: func(r domain.Gate) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		signals:       table[domain.Signal]{key: func(r domain.Signal) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		workers:       table[domain.Worker]{key: func(r domain.Worker) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		runs:          table[domain.WorkflowRun]{key: func(r domain.WorkflowRun) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		steps:         table[domain.StepRun]{key: func(r domain.StepRun) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		tokens:        table[domain.MCPToken]{key: func(r domain.MCPToken) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		audit:         table[domain.AuditEntry]{key: func(r domain.AuditEntry) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		artifacts:     table[domain.Artifact]{key: func(r domain.Artifact) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		verifications: table[domain.VerificationReport]{key: func(r domain.VerificationReport) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		assumptions:   table[domain.Assumption]{key: func(r domain.Assumption) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		decisions:     table[domain.Decision]{key: func(r domain.Decision) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		kbProposals:   table[domain.KnowledgeProposal]{key: func(r domain.KnowledgeProposal) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		testRuns:      table[domain.TestRun]{key: func(r domain.TestRun) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		testResult:    table[domain.TestResult]{key: func(r domain.TestResult) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		councils:      table[domain.Council]{key: func(r domain.Council) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		proposals:     table[domain.Proposal]{key: func(r domain.Proposal) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		council:       table[domain.CouncilContribution]{key: func(r domain.CouncilContribution) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		deliveries:    map[string]domain.WebhookDelivery{},
		counters:      map[counterKey]int64{},
		jobMCP:        map[uuid.UUID]jobMCPHash{},
		knowledge:     table[domain.KnowledgeEntry]{key: func(r domain.KnowledgeEntry) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		embeddings:    map[uuid.UUID][]float32{},
		identity:      newIdentityData(),
		access:        newAccessData(),
		providers:     table[domain.CapacityProvider]{key: func(r domain.CapacityProvider) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		shapes:        table[domain.Shape]{key: func(r domain.Shape) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		machines:      table[domain.Machine]{key: func(r domain.Machine) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		bootstrap:     table[domain.BootstrapToken]{key: func(r domain.BootstrapToken) (uuid.UUID, uuid.UUID) { return r.AccountID, r.MachineID }},
		joinCodes:     table[domain.ProviderJoinCode]{key: func(r domain.ProviderJoinCode) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		policies:      table[domain.FleetPolicy]{key: func(r domain.FleetPolicy) (uuid.UUID, uuid.UUID) { return r.AccountID, r.AccountID }},
		trackers:      newTrackerData(),
		images:        table[domain.Image]{key: func(r domain.Image) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		configPlans:   table[domain.ConfigPlan]{key: func(r domain.ConfigPlan) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		settings:      table[domain.ConfigSetting]{key: func(r domain.ConfigSetting) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		secrets:       table[domain.StoredSecret]{key: func(r domain.StoredSecret) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		modelConns:    table[domain.ModelConnection]{key: func(r domain.ModelConnection) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		modelAssigns:  table[domain.ModelAssignment]{key: func(r domain.ModelAssignment) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		modelAuths:    table[domain.ModelAuthorization]{key: func(r domain.ModelAuthorization) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		conversations: table[domain.AssistantConversation]{key: func(r domain.AssistantConversation) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		messages:      table[domain.AssistantMessage]{key: func(r domain.AssistantMessage) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
	}}}
}

func (d *storeData) clone() *storeData {
	c := *d
	c.trackers = d.trackers.clone()
	c.accounts = d.accounts.clone()
	c.projects = d.projects.clone()
	c.goals = d.goals.clone()
	c.specs = d.specs.clone()
	c.tasks = d.tasks.clone()
	c.changes = d.changes.clone()
	c.jobs = d.jobs.clone()
	c.jobEvents = d.jobEvents.clone()
	c.gates = d.gates.clone()
	c.signals = d.signals.clone()
	c.workers = d.workers.clone()
	c.runs = d.runs.clone()
	c.steps = d.steps.clone()
	c.tokens = d.tokens.clone()
	c.audit = d.audit.clone()
	c.artifacts = d.artifacts.clone()
	c.testRuns = d.testRuns.clone()
	c.testResult = d.testResult.clone()
	c.verifications = d.verifications.clone()
	c.assumptions = d.assumptions.clone()
	c.decisions = d.decisions.clone()
	c.knowledge = d.knowledge.clone()
	c.kbProposals = d.kbProposals.clone()
	c.embeddings = maps.Clone(d.embeddings)
	c.councils = d.councils.clone()
	c.proposals = d.proposals.clone()
	c.council = d.council.clone()
	c.providers = d.providers.clone()
	c.shapes = d.shapes.clone()
	c.machines = d.machines.clone()
	c.bootstrap = d.bootstrap.clone()
	c.joinCodes = d.joinCodes.clone()
	c.policies = d.policies.clone()
	c.deliveries = maps.Clone(d.deliveries)
	c.counters = maps.Clone(d.counters)
	c.jobMCP = maps.Clone(d.jobMCP)
	c.identity = d.identity.clone()
	c.access = d.access.clone()
	c.images = d.images.clone()
	c.configPlans = d.configPlans.clone()
	c.settings = d.settings.clone()
	c.secrets = d.secrets.clone()
	c.modelConns = d.modelConns.clone()
	c.modelAssigns = d.modelAssigns.clone()
	c.modelAuths = d.modelAuths.clone()
	c.conversations = d.conversations.clone()
	c.messages = d.messages.clone()
	return &c
}

// WithTx implements store.Store. Nested calls join the running transaction.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx store.Store) error) error {
	if s.inTx {
		return fn(ctx, s)
	}
	s.shared.txMu.Lock()
	defer s.shared.txMu.Unlock()
	s.shared.mu.Lock()
	snapshot := s.shared.d.clone()
	s.shared.mu.Unlock()
	if err := fn(ctx, &Store{shared: s.shared, inTx: true}); err != nil {
		s.shared.mu.Lock()
		s.shared.d = snapshot
		s.shared.mu.Unlock()
		return err
	}
	return nil
}

// do runs fn with exclusive access to the data.
func (s *Store) do(fn func(d *storeData, now time.Time) error) error {
	if !s.inTx {
		s.shared.txMu.Lock()
		defer s.shared.txMu.Unlock()
	}
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	return fn(s.shared.d, s.shared.now())
}

// nextNumber allocates the next human ref number of an account.
func (d *storeData) nextNumber(account uuid.UUID, kind domain.RefKind) int64 {
	k := counterKey{account, kind}
	d.counters[k]++
	return d.counters[k]
}

func newID(id uuid.UUID) uuid.UUID {
	if id == uuid.Nil() {
		return domain.NewID()
	}
	return id
}

// table is an ordered list of rows, oldest first.
type table[T any] struct {
	rows []T
	key  func(T) (account, id uuid.UUID)
}

func (t table[T]) clone() table[T] {
	t.rows = slices.Clone(t.rows)
	return t
}

func (t *table[T]) insert(r T) { t.rows = append(t.rows, r) }

// find returns a pointer to the first matching row.
func (t *table[T]) find(match func(T) bool) (*T, error) {
	i := slices.IndexFunc(t.rows, match)
	if i < 0 {
		return nil, store.ErrNotFound
	}
	return &t.rows[i], nil
}

// get finds a row of the account by ID.
func (t *table[T]) get(account, id uuid.UUID) (*T, error) {
	r, err := t.find(func(r T) bool {
		a, i := t.key(r)
		return a == account && i == id
	})
	if err != nil {
		return nil, fmt.Errorf("%T %s: %w", *new(T), id, err)
	}
	return r, nil
}

// filter returns up to limit matching rows (0 = all), oldest first.
func (t *table[T]) filter(limit int, match func(T) bool) []T {
	out := []T{}
	for _, r := range t.rows {
		if match(r) {
			out = append(out, r)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out
}

// newest returns up to limit matching rows (0 = all), newest first.
func (t *table[T]) newest(limit int, match func(T) bool) []T {
	out := []T{}
	for _, r := range slices.Backward(t.rows) {
		if match(r) {
			out = append(out, r)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out
}

// exists reports whether a row matches.
func (t *table[T]) exists(match func(T) bool) bool {
	return slices.ContainsFunc(t.rows, match)
}

// status is a domain status type with a transition table.
type status[S any] interface {
	~string
	Transition(to S) (S, error)
}

// casStatus implements the UpdateStatus contract on *cur.
func casStatus[S status[S]](cur *S, from, to S) error {
	if _, err := from.Transition(to); err != nil {
		return err
	}
	if *cur != from {
		return fmt.Errorf("status is %q, not %q: %w", *cur, from, store.ErrConflict)
	}
	*cur = to
	return nil
}

// zeroOr reports whether filter is unset or equal to v.
func zeroOr[V comparable](filter, v V) bool {
	var zero V
	return filter == zero || filter == v
}

// read runs fn and returns its value.
func read[V any](s *Store, fn func(d *storeData) (V, error)) (V, error) {
	var v V
	err := s.do(func(d *storeData, _ time.Time) error {
		var err error
		v, err = fn(d)
		return err
	})
	return v, err
}

// getRow returns a copy of a row by account and ID.
func getRow[T any](s *Store, t func(*storeData) *table[T], account, id uuid.UUID) (T, error) {
	return read(s, func(d *storeData) (T, error) {
		r, err := t(d).get(account, id)
		if err != nil {
			var zero T
			return zero, err
		}
		return *r, nil
	})
}

// findRow returns a copy of the first matching row.
func findRow[T any](s *Store, t func(*storeData) *table[T], match func(T) bool) (T, error) {
	return read(s, func(d *storeData) (T, error) {
		r, err := t(d).find(match)
		if err != nil {
			var zero T
			return zero, err
		}
		return *r, nil
	})
}

// listRows returns matching rows, oldest first.
func listRows[T any](s *Store, t func(*storeData) *table[T], limit int, match func(T) bool) ([]T, error) {
	return read(s, func(d *storeData) ([]T, error) { return t(d).filter(limit, match), nil })
}

// update applies fn to a row by account and ID.
func update[T any](s *Store, t func(*storeData) *table[T], account, id uuid.UUID, fn func(r *T, now time.Time) error) error {
	return s.do(func(d *storeData, now time.Time) error {
		r, err := t(d).get(account, id)
		if err != nil {
			return err
		}
		return fn(r, now)
	})
}

// foreign reports ErrNotFound when id names a row of another account, like
// the Postgres store, which checks every reference against the account.
// Unknown IDs pass: tests build rows with made-up parents.
func foreign[T any](t *table[T], account, id uuid.UUID) error {
	if id == uuid.Nil() {
		return nil
	}
	r, err := t.find(func(r T) bool { _, i := t.key(r); return i == id })
	if err != nil {
		return nil
	}
	if a, _ := t.key(*r); a != account {
		return fmt.Errorf("%T %s: %w", *new(T), id, store.ErrNotFound)
	}
	return nil
}

// firstErr returns the first non-nil error.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
