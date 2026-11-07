package tracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// DefaultMaxActiveGoals is the default of trackers.max_active_goals.
const DefaultMaxActiveGoals = 3

// How often Run looks at its links and the outbox, and polls the trackers.
const (
	SyncEvery = 15 * time.Second
	PollEvery = 2 * time.Minute
)

const (
	queueSize = 256
	dueBatch  = 100
)

// Runs starts and cancels playbook runs; *workflow.Engine implements it.
type Runs interface {
	Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error)
	CancelRun(ctx context.Context, accountID, runID uuid.UUID, reason string) error
}

// Config holds the dependencies of a Service.
type Config struct {
	Store    store.Store
	Registry *Registry
	Gates    gates.Service
	Authz    *authz.Engine
	Runs     Runs
	// Playbook is the playbook a goal of this kind runs in a project.
	Playbook func(domain.Project, domain.GoalKind) string
	// MaxActiveGoals is trackers.max_active_goals.
	MaxActiveGoals int
	// PublicURL starts the links to goals in comments.
	PublicURL string
	Logger    *slog.Logger
	Now       func() time.Time
}

// Service is the tracker-neutral core.
type Service struct {
	cfg Config
	// mu keeps one change of tickets at a time: forecrewd is a single
	// replica. The database's unique indexes stand behind it.
	mu    sync.Mutex
	queue chan queued
}

type queued struct {
	conn domain.TrackerConnection
	ev   Event
}

// New returns the service.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.MaxActiveGoals == 0 {
		cfg.MaxActiveGoals = DefaultMaxActiveGoals
	}
	return &Service{cfg: cfg, queue: make(chan queued, queueSize)}
}

// Registry returns the adapters.
func (s *Service) Registry() *Registry { return s.cfg.Registry }

// Run processes events, syncs and polls until ctx is done.
func (s *Service) Run(ctx context.Context) {
	sync, poll := time.NewTicker(SyncEvery), time.NewTicker(PollEvery)
	defer sync.Stop()
	defer poll.Stop()
	s.logged(ctx, "poll trackers", s.Poll)
	s.logged(ctx, "sync tickets", s.Sync)
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-s.queue:
			s.process(ctx, q)
		case <-sync.C:
			s.logged(ctx, "sync tickets", s.Sync)
		case <-poll.C:
			s.logged(ctx, "poll trackers", s.Poll)
		}
	}
}

func (s *Service) logged(ctx context.Context, what string, fn func(context.Context) error) {
	if err := fn(ctx); err != nil && ctx.Err() == nil {
		s.cfg.Logger.Error(what, "error", err)
	}
}

// process handles a queued event once: a delivery seen before is skipped.
func (s *Service) process(ctx context.Context, q queued) {
	if q.ev.ID != "" {
		isNew, err := s.cfg.Store.WebhookDeliveries().Record(ctx, domain.WebhookDelivery{
			DeliveryID: q.conn.ID.String() + ":" + q.ev.ID, Event: "tracker", ReceivedAt: s.cfg.Now(),
		})
		if err != nil {
			s.cfg.Logger.Error("record tracker delivery", "connection", q.conn.ID, "error", err)
			return
		}
		if !isNew {
			return
		}
	}
	if err := s.Hint(ctx, q.conn, q.ev); err != nil {
		s.cfg.Logger.Error("process ticket event", "account_id", q.conn.AccountID, "connection", q.conn.ID, "key", q.ev.Key, "error", err)
	}
}

// Deliver takes a delivery of a webhook the connections of kind share, such as
// GitHub's. Each connection's adapter checks the signature and picks what is
// its own. The tickets are read later, by Run, so the caller never waits for a
// tracker; an event that finds the queue full is left to polling.
func (s *Service) Deliver(ctx context.Context, kind string, r Request) error {
	conns, err := s.cfg.Store.TrackerConnections().ListByKind(ctx, kind)
	if err != nil {
		return fmt.Errorf("list %s connections: %w", kind, err)
	}
	var errs []error
	for _, c := range conns {
		ad, err := s.cfg.Registry.Build(c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		events, err := ad.ParseEvent(r)
		if err != nil {
			errs = append(errs, fmt.Errorf("connection %s: %w", c.Name, err))
			continue
		}
		for _, ev := range events {
			if !s.enqueueEvent(c, ev) {
				s.cfg.Logger.Warn("ticket queue full", "connection", c.ID, "key", ev.Key)
			}
		}
	}
	return errors.Join(errs...)
}

// enqueueEvent queues an event for Run, and reports whether there was room.
func (s *Service) enqueueEvent(c domain.TrackerConnection, ev Event) bool {
	select {
	case s.queue <- queued{conn: c, ev: ev}:
		return true
	default:
		return false
	}
}

// Drain handles the queued events now, as Run does.
func (s *Service) Drain(ctx context.Context) {
	for {
		select {
		case q := <-s.queue:
			s.process(ctx, q)
		default:
			return
		}
	}
}

// Poll asks every connection what changed. A cursor moves only when all of
// its tickets went through, so polling alone gives what webhooks give.
func (s *Service) Poll(ctx context.Context) error {
	accounts, err := s.cfg.Store.Accounts().List(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	var errs []error
	for _, a := range accounts {
		conns, err := s.cfg.Store.TrackerConnections().List(ctx, a.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, c := range conns {
			if err := s.pollOne(ctx, c); err != nil {
				errs = append(errs, fmt.Errorf("poll %s: %w", c.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (s *Service) pollOne(ctx context.Context, c domain.TrackerConnection) error {
	ad, err := s.cfg.Registry.Build(c)
	if err != nil {
		return err
	}
	cursors := s.cfg.Store.TrackerCursors()
	cursor, err := cursors.Get(ctx, c.AccountID, c.ID)
	if err != nil {
		return err
	}
	events, next, err := ad.Changes(ctx, cursor)
	if err != nil {
		return err
	}
	var errs []error
	for _, ev := range events {
		errs = append(errs, s.Hint(ctx, c, ev))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if next == cursor {
		return nil
	}
	return cursors.Set(ctx, c.AccountID, c.ID, next)
}

// Hint looks at the ticket an event is about. The event says which ticket and
// nothing more: what happens depends on what Get returns.
func (s *Service) Hint(ctx context.Context, c domain.TrackerConnection, ev Event) error {
	if ev.Self || ev.Key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ad, err := s.cfg.Registry.Build(c)
	if err != nil {
		return err
	}
	t, err := s.read(ctx, c, ad, ev.Key)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	link, err := s.cfg.Store.TicketLinks().GetLive(ctx, c.AccountID, c.ID, ev.Key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return s.intake(ctx, c, ad, t)
	case err != nil:
		return fmt.Errorf("find the link of %s: %w", ev.Key, err)
	}
	switch link.Status {
	case domain.LinkWaiting:
		return s.reconsider(ctx, c, ad, link, t)
	case domain.LinkQueued:
		if t.Closed {
			return s.end(ctx, &link, domain.LinkCanceled)
		}
	case domain.LinkActive:
		return s.follow(ctx, c, ad, link, t)
	}
	return nil
}

// read gets a ticket within the bounds.
func (s *Service) read(ctx context.Context, c domain.TrackerConnection, ad Adapter, key string) (Ticket, error) {
	t, err := ad.Get(ctx, key)
	if err != nil {
		return Ticket{}, fmt.Errorf("read ticket %s: %w", key, err)
	}
	t = bound(t)
	t.Ref = Ref{Connection: c.ID, Key: key}
	return t, nil
}

// contentHash identifies what a ticket says.
func contentHash(t Ticket) string {
	sum := sha256.Sum256([]byte(t.Title + "\x00" + t.Body))
	return hex.EncodeToString(sum[:])
}

// lastComment is the cursor of a ticket's comments.
func lastComment(t Ticket) string {
	if len(t.Comments) == 0 {
		return ""
	}
	return t.Comments[len(t.Comments)-1].ID
}

// person finds the Forecrew user of a tracker user: a mapping first, then an
// identity the adapter knows of.
func (s *Service) person(ctx context.Context, c domain.TrackerConnection, ad Adapter, u User) (uuid.UUID, bool, error) {
	if u.ID == "" || u.Bot {
		return uuid.Nil(), false, nil
	}
	i, err := s.cfg.Store.TrackerIdentities().Find(ctx, c.AccountID, c.ID, u.ID)
	switch {
	case err == nil:
		return i.UserID, true, nil
	case !errors.Is(err, store.ErrNotFound):
		return uuid.Nil(), false, fmt.Errorf("find the user of %s: %w", u.ID, err)
	}
	if l, ok := ad.(Linker); ok {
		return l.LinkedUser(ctx, c.AccountID, u)
	}
	return uuid.Nil(), false, nil
}

// audit records what happened to a ticket.
func audit(ctx context.Context, st store.Store, by domain.Actor, l domain.TicketLink, action string, now time.Time, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal audit data: %w", err)
	}
	return st.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: l.AccountID, ProjectID: l.ProjectID, Actor: by.String(), Credential: by.Credential.String(),
		Action: action, SubjectType: "ticket", SubjectID: l.ID, Data: raw, CreatedAt: now,
	})
}

func (s *Service) forecrewd(accountID uuid.UUID) domain.Actor {
	return domain.SystemActor(accountID, domain.ForecrewdName)
}
