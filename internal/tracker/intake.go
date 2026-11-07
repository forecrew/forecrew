package tracker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// verdict is where a handed-over ticket goes.
type verdict struct {
	by      Handover
	project uuid.UUID
	route   uuid.UUID
	user    uuid.UUID
	reason  domain.TicketWaitReason // empty: it may start
}

// routeOf applies the routes to t: all those that match must name one
// project, and the first of them gives the route. Nothing is guessed.
func (s *Service) routeOf(ctx context.Context, c domain.TrackerConnection, t Ticket, v *verdict) error {
	routes, err := s.cfg.Store.TrackerRoutes().List(ctx, c.AccountID, c.ID)
	if err != nil {
		return fmt.Errorf("list routes: %w", err)
	}
	routes = slices.DeleteFunc(routes, func(r domain.TrackerRoute) bool { return !matches(r.Match, t) })
	switch {
	case len(routes) == 0:
		v.reason = domain.WaitNoRoute
	case slices.ContainsFunc(routes, func(r domain.TrackerRoute) bool { return r.ProjectID != routes[0].ProjectID }):
		v.reason = domain.WaitManyProjects
	default:
		v.project, v.route = routes[0].ProjectID, routes[0].ID
	}
	return nil
}

// judge decides where a ticket goes, or reports that nobody handed it over.
func (s *Service) judge(ctx context.Context, c domain.TrackerConnection, ad Adapter, t Ticket) (verdict, bool, error) {
	h, ok := handover(c.Triggers, t)
	if !ok || t.Closed {
		return verdict{}, false, nil
	}
	v := verdict{by: h}
	if err := s.routeOf(ctx, c, t, &v); err != nil || v.reason != "" {
		return v, true, err
	}
	user, found, err := s.person(ctx, c, ad, h.By)
	if err != nil {
		return v, true, err
	}
	if !found {
		v.reason = domain.WaitUnknownPerson
		return v, true, nil
	}
	allowed, err := s.mayStart(ctx, c.AccountID, user, v.project)
	if err != nil {
		return v, true, err
	}
	if !allowed {
		v.reason = domain.WaitNotPermitted
		return v, true, nil
	}
	v.user = user
	return v, true, nil
}

// mayStart reports whether a user may create goals in a project.
func (s *Service) mayStart(ctx context.Context, accountID, user, project uuid.UUID) (bool, error) {
	by := domain.UserActor(accountID, user, domain.Credential{})
	err := s.cfg.Authz.Authorize(ctx, by, domain.PermGoalCreate, authz.Project(accountID, project))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, authz.ErrDenied):
		return false, nil
	}
	return false, fmt.Errorf("check who may start: %w", err)
}

// intake looks at a ticket without a live link. Only a person's handover
// makes one, and after a link has ended only a handover newer than its end.
func (s *Service) intake(ctx context.Context, c domain.TrackerConnection, ad Adapter, t Ticket) error {
	v, handed, err := s.judge(ctx, c, ad, t)
	if err != nil || !handed {
		return err
	}
	last, err := s.cfg.Store.TicketLinks().LastEnded(ctx, c.AccountID, c.ID, t.Ref.Key)
	switch {
	case err == nil:
		if !v.by.At.After(last.UpdatedAt) {
			return nil
		}
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("find the last link of %s: %w", t.Ref.Key, err)
	}
	l := domain.TicketLink{
		AccountID: c.AccountID, ConnectionID: c.ID, Key: t.Ref.Key, Title: t.Title, URL: t.URL, Status: domain.LinkQueued,
		ProjectID: v.project, RouteID: v.route, UserID: v.user, Reason: v.reason, HandedBy: v.by.By.Name,
		ContentHash: contentHash(t), CommentCursor: lastComment(t),
	}
	if v.reason != "" {
		l.Status = domain.LinkWaiting
	}
	if l.IssueNumber, err = s.issueNumber(ctx, l.AccountID, l.ProjectID, t); err != nil {
		return err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.TicketLinks().Create(ctx, &l); err != nil {
			return err
		}
		return audit(ctx, tx, s.forecrewd(c.AccountID), l, "ticket.received", s.cfg.Now(), map[string]any{"key": l.Key, "status": l.Status, "reason": l.Reason})
	})
	switch {
	case errors.Is(err, store.ErrConflict): // a redelivery got here first
		return nil
	case err != nil:
		return fmt.Errorf("link ticket %s: %w", l.Key, err)
	}
	if l.Status == domain.LinkQueued {
		return s.fill(ctx, l.AccountID, l.ProjectID)
	}
	if err := s.say(ctx, s.cfg.Store, l, "waiting", "Forecrew has this ticket in its Inbox: a member has to accept it before work starts."); err != nil {
		return err
	}
	return s.ensureGate(ctx, c, &l, t)
}

// issueNumber is the number of the GitHub issue a ticket is, when that issue
// is in the repo of the project: from then on the issue is taken, and neither
// a bug report nor another ticket gets a goal on it. Zero otherwise.
func (s *Service) issueNumber(ctx context.Context, accountID, project uuid.UUID, t Ticket) (int, error) {
	if t.Issue == nil || project == uuid.Nil() {
		return 0, nil
	}
	p, err := s.cfg.Store.Projects().Get(ctx, accountID, project)
	if err != nil {
		return 0, fmt.Errorf("get project: %w", err)
	}
	if !strings.EqualFold(t.Issue.Repo, p.Repo) {
		return 0, nil
	}
	return t.Issue.Number, nil
}

// ticketText quotes a ticket, marked as data.
func ticketText(t Ticket) string {
	return contextpack.WrapUntrusted("ticket "+t.Ref.Key, clip(t.Title+"\n\n"+t.Body, 8<<10))
}

// ensureGate opens the ticket_intake gate of a waiting ticket with a project.
func (s *Service) ensureGate(ctx context.Context, c domain.TrackerConnection, l *domain.TicketLink, t Ticket) error {
	if l.ProjectID == uuid.Nil() || l.GateID != uuid.Nil() {
		return nil
	}
	p, err := s.cfg.Store.Projects().Get(ctx, l.AccountID, l.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	why := map[domain.TicketWaitReason]string{
		domain.WaitUnknownPerson: "Whoever handed it over is not a Forecrew user we know.",
		domain.WaitNotPermitted:  "Whoever handed it over may not create goals in this project.",
	}[l.Reason]
	_, err = s.cfg.Gates.Open(ctx, gates.OpenRequest{
		AccountID: l.AccountID, ProjectID: l.ProjectID, Kind: domain.GateTicketIntake, SubjectRepo: p.Repo,
		Title:   "Accept ticket " + l.Key + " from " + c.Name,
		Summary: strings.TrimSpace(why+" Approve to start a goal from this ticket as yourself; reject to dismiss it. "+l.URL) + "\n\n" + ticketText(t),
		Opened: func(ctx context.Context, tx store.Store, g domain.Gate) error {
			l.GateID = g.ID
			return tx.TicketLinks().Update(ctx, l)
		},
	})
	if err != nil {
		return fmt.Errorf("open the gate of %s: %w", l.Key, err)
	}
	return nil
}

// closeGate rejects the pending intake gate of a ticket that left the Inbox
// another way, as forecrewd.
func (s *Service) closeGate(ctx context.Context, l *domain.TicketLink, why string) error {
	if l.GateID == uuid.Nil() {
		return nil
	}
	id := l.GateID
	l.GateID = uuid.Nil()
	g, err := s.cfg.Gates.Get(ctx, l.AccountID, id)
	if err != nil {
		return fmt.Errorf("get the gate of %s: %w", l.Key, err)
	}
	if g.Status != domain.GatePending {
		return nil
	}
	if _, err := s.cfg.Gates.Resolve(ctx, s.forecrewd(l.AccountID), id, domain.GateRejected, why); err != nil {
		return fmt.Errorf("close the gate of %s: %w", l.Key, err)
	}
	return nil
}

// reconsider looks again at a ticket in the Inbox: closed, it leaves; handed
// over by someone who may start, it starts; otherwise it waits with what is
// known now.
func (s *Service) reconsider(ctx context.Context, c domain.TrackerConnection, ad Adapter, l domain.TicketLink, t Ticket) error {
	if t.Closed {
		if err := s.closeGate(ctx, &l, "The ticket was closed."); err != nil {
			return err
		}
		return s.end(ctx, &l, domain.LinkCanceled)
	}
	v, handed, err := s.judge(ctx, c, ad, t)
	if err != nil || !handed {
		return err
	}
	if l.IssueNumber, err = s.issueNumber(ctx, l.AccountID, v.project, t); err != nil {
		return err
	}
	if v.reason != "" {
		if v.project != l.ProjectID {
			if err := s.closeGate(ctx, &l, "The ticket's route changed."); err != nil {
				return err
			}
		}
		l.Reason, l.ProjectID, l.RouteID, l.HandedBy, l.Title = v.reason, v.project, v.route, v.by.By.Name, t.Title
		if err := s.update(ctx, &l); err != nil {
			return err
		}
		return s.ensureGate(ctx, c, &l, t)
	}
	if err := s.closeGate(ctx, &l, "A person who may start it handed the ticket over."); err != nil {
		return err
	}
	l.Status, l.Reason, l.ProjectID, l.RouteID, l.UserID, l.HandedBy = domain.LinkQueued, "", v.project, v.route, v.user, v.by.By.Name
	if err := s.update(ctx, &l); err != nil {
		return err
	}
	return s.fill(ctx, l.AccountID, l.ProjectID)
}

func (s *Service) update(ctx context.Context, l *domain.TicketLink) error {
	if err := s.cfg.Store.TicketLinks().Update(ctx, l); err != nil {
		return fmt.Errorf("update the link of %s: %w", l.Key, err)
	}
	return nil
}

// end closes a link.
func (s *Service) end(ctx context.Context, l *domain.TicketLink, status domain.TicketLinkStatus) error {
	l.Status, l.Reason = status, ""
	return s.update(ctx, l)
}

// fill starts the queued tickets of a project, oldest first, up to the
// limit; the others get one comment saying they wait.
func (s *Service) fill(ctx context.Context, accountID, project uuid.UUID) error {
	links := s.cfg.Store.TicketLinks()
	active, err := links.CountActive(ctx, accountID, project)
	if err != nil {
		return fmt.Errorf("count active tickets: %w", err)
	}
	waiting, err := links.List(ctx, accountID, store.TicketLinkFilter{ProjectID: project, Status: domain.LinkQueued})
	if err != nil {
		return fmt.Errorf("list queued tickets: %w", err)
	}
	var errs []error
	for _, l := range waiting {
		if active < s.cfg.MaxActiveGoals {
			started, err := s.start(ctx, l)
			if started {
				active++
			}
			errs = append(errs, err)
			continue
		}
		errs = append(errs, s.hold(ctx, l))
	}
	return errors.Join(errs...)
}

// hold tells a queued ticket it waits for a place.
func (s *Service) hold(ctx context.Context, l domain.TicketLink) error {
	if l.State == domain.TicketQueued {
		return nil
	}
	text := fmt.Sprintf("Forecrew will start this ticket when a place is free: a project works on %d tickets at a time.", s.cfg.MaxActiveGoals)
	return s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := s.say(ctx, tx, l, "queued", text); err != nil {
			return err
		}
		return s.setState(ctx, tx, &l, domain.TicketQueued)
	})
}

// starter returns who a queued ticket starts as: the user it came with, if
// they still may, or whoever approved its gate, whose permission the gate
// checked. A zero actor means nobody may any more.
func (s *Service) starter(ctx context.Context, l domain.TicketLink) (domain.Actor, error) {
	if l.UserID != uuid.Nil() {
		ok, err := s.mayStart(ctx, l.AccountID, l.UserID, l.ProjectID)
		if err != nil || !ok {
			return domain.Actor{}, err
		}
		return domain.UserActor(l.AccountID, l.UserID, domain.Credential{}), nil
	}
	g, err := s.cfg.Gates.Get(ctx, l.AccountID, l.GateID)
	if err != nil {
		return domain.Actor{}, fmt.Errorf("get the gate of %s: %w", l.Key, err)
	}
	by, err := domain.ParseActor(g.ResolvedBy)
	if err != nil {
		return domain.Actor{}, err
	}
	by.AccountID = l.AccountID
	return by, nil
}

// start gives a queued ticket its goal. The ticket is read again: it may have
// closed while it waited, and the goal takes what it says now.
func (s *Service) start(ctx context.Context, l domain.TicketLink) (bool, error) {
	c, err := s.cfg.Store.TrackerConnections().Get(ctx, l.AccountID, l.ConnectionID)
	if err != nil {
		return false, fmt.Errorf("get connection: %w", err)
	}
	ad, err := s.cfg.Registry.Build(c)
	if err != nil {
		return false, err
	}
	t, err := s.read(ctx, c, ad, l.Key)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, s.end(ctx, &l, domain.LinkCanceled)
	case err != nil:
		return false, err
	case t.Closed:
		return false, s.end(ctx, &l, domain.LinkCanceled)
	}
	by, err := s.starter(ctx, l)
	if err != nil {
		return false, err
	}
	if by.Kind == "" {
		l.Status, l.Reason, l.UserID, l.GateID = domain.LinkWaiting, domain.WaitNotPermitted, uuid.Nil(), uuid.Nil()
		if err := s.update(ctx, &l); err != nil {
			return false, err
		}
		return false, s.ensureGate(ctx, c, &l, t)
	}
	return s.launch(ctx, l, t, by)
}

// typeKind is the goal kind of a ticket's type: a bug type gives bug, anything
// else feature.
func typeKind(t Ticket) domain.GoalKind {
	if strings.EqualFold(t.Type, string(domain.GoalBug)) {
		return domain.GoalBug
	}
	return domain.GoalFeature
}

// goalPlan is how a ticket's goal runs: the route decides, and otherwise kind
// and the project do.
func (s *Service) goalPlan(ctx context.Context, l domain.TicketLink, kind domain.GoalKind, p domain.Project) (domain.GoalKind, string, error) {
	playbook := ""
	if l.RouteID != uuid.Nil() {
		r, err := s.cfg.Store.TrackerRoutes().Get(ctx, l.AccountID, l.RouteID)
		if err != nil {
			return "", "", fmt.Errorf("get route: %w", err)
		}
		if r.GoalKind != "" {
			kind = r.GoalKind
		}
		playbook = r.Playbook
	}
	if playbook == "" {
		playbook = s.cfg.Playbook(p, kind)
	}
	return kind, playbook, nil
}

// launch creates the goal of a ticket and starts its playbook, and reports
// whether the ticket now holds a place under the limit. The run has the link's
// ID, so Sync can start it again after a crash.
func (s *Service) launch(ctx context.Context, l domain.TicketLink, t Ticket, by domain.Actor) (bool, error) {
	p, err := s.cfg.Store.Projects().Get(ctx, l.AccountID, l.ProjectID)
	if err != nil {
		return false, fmt.Errorf("get project: %w", err)
	}
	if p.Status != domain.ProjectOnboarded {
		return false, fmt.Errorf("ticket %s: project %s is %s, not onboarded", l.Key, p.Name, p.Status)
	}
	kind, playbook, err := s.goalPlan(ctx, l, typeKind(t), p)
	if err != nil {
		return false, err
	}
	title := t.Title
	if title == "" {
		title = l.Key
	}
	g := domain.Goal{
		AccountID: l.AccountID, ProjectID: p.ID, Kind: kind, Title: title, Status: domain.GoalDraft, CreatedBy: by.String(),
		Body: fmt.Sprintf("From ticket %s: %s\n\n%s", l.Key, t.URL, t.Body),
	}
	if g.IssueNumber, err = s.issueNumber(ctx, l.AccountID, p.ID, t); err != nil {
		return false, err
	}
	l.IssueNumber = g.IssueNumber
	if holder, ok, err := s.issueHolder(ctx, p, g.IssueNumber); err != nil {
		return false, err
	} else if ok {
		return false, s.refuseDuplicate(ctx, l, holder)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Goals().Create(ctx, &g); err != nil {
			return err
		}
		l.GoalID, l.Status, l.Reason, l.Title = g.ID, domain.LinkActive, "", t.Title
		l.ContentHash, l.CommentCursor = contentHash(t), lastComment(t)
		if err := s.setState(ctx, tx, &l, domain.TicketInProgress); err != nil {
			return err
		}
		if err := s.say(ctx, tx, l, "goal", fmt.Sprintf("Forecrew is working on this as %s: %s", g.Ref(), s.goalURL(g))); err != nil {
			return err
		}
		return audit(ctx, tx, by, l, "ticket.started", s.cfg.Now(), map[string]any{"key": l.Key, "goal": g.Ref()})
	})
	if err != nil {
		return false, fmt.Errorf("start the goal of %s: %w", l.Key, err)
	}
	return true, s.startRun(ctx, l, g, p, playbook)
}

// issueHolder returns the goal at work on an issue of p. A ticket routed only
// now cannot have reserved the issue, and "@forecrew bug" can have got there
// first.
func (s *Service) issueHolder(ctx context.Context, p domain.Project, number int) (domain.Goal, bool, error) {
	if number == 0 {
		return domain.Goal{}, false, nil
	}
	goals, err := s.cfg.Store.Goals().List(ctx, p.AccountID, store.GoalFilter{ProjectID: p.ID})
	if err != nil {
		return domain.Goal{}, false, fmt.Errorf("list the goals of %s: %w", p.Name, err)
	}
	i := slices.IndexFunc(goals, func(g domain.Goal) bool { return g.IssueNumber == number && !g.Status.Terminal() })
	if i < 0 {
		return domain.Goal{}, false, nil
	}
	return goals[i], true, nil
}

// refuseDuplicate ends a ticket whose issue another goal works on: one issue
// has one goal.
func (s *Service) refuseDuplicate(ctx context.Context, l domain.TicketLink, holder domain.Goal) error {
	return s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		l.Status, l.Reason = domain.LinkCanceled, ""
		if err := tx.TicketLinks().Update(ctx, &l); err != nil {
			return fmt.Errorf("update the link of %s: %w", l.Key, err)
		}
		return s.say(ctx, tx, l, "duplicate", fmt.Sprintf("%s works on this issue already: %s", holder.Ref(), s.goalURL(holder)))
	})
}

func (s *Service) startRun(ctx context.Context, l domain.TicketLink, g domain.Goal, p domain.Project, playbook string) error {
	_, err := s.cfg.Runs.Start(ctx, playbook, workflow.StartRequest{
		ID: l.ID, AccountID: l.AccountID, ProjectID: p.ID, GoalID: g.ID,
		Inputs: map[string]any{"repo": p.Repo, "goal_id": g.ID.String(), "platforms": []string{}},
	})
	if err != nil {
		return fmt.Errorf("start the plan of %s: %w", g.Ref(), err)
	}
	return nil
}

func (s *Service) goalURL(g domain.Goal) string { return s.cfg.PublicURL + "/goals/" + g.Ref() }

// Accept takes a waiting ticket into project as by, who must be a person; the
// caller has checked by's permission. It queues the ticket and starts it if
// there is a place.
func (s *Service) Accept(ctx context.Context, by domain.Actor, id, project uuid.UUID) (domain.TicketLink, error) {
	user, ok := by.SubjectID()
	if !ok || by.Kind != domain.ActorUser {
		return domain.TicketLink{}, fmt.Errorf("only a person accepts a ticket: %w", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, c, ad, err := s.waiting(ctx, by.AccountID, id)
	if err != nil {
		return domain.TicketLink{}, err
	}
	t, err := s.read(ctx, c, ad, l.Key)
	if err != nil {
		return domain.TicketLink{}, err
	}
	routes, err := s.cfg.Store.TrackerRoutes().List(ctx, l.AccountID, l.ConnectionID)
	if err != nil {
		return domain.TicketLink{}, fmt.Errorf("list routes: %w", err)
	}
	l.RouteID = uuid.Nil()
	if i := slices.IndexFunc(routes, func(r domain.TrackerRoute) bool { return r.ProjectID == project && matches(r.Match, t) }); i >= 0 {
		l.RouteID = routes[i].ID
	}
	if err := s.settleGate(ctx, by, &l, domain.GateApproved, "Accepted."); err != nil {
		return domain.TicketLink{}, err
	}
	l.Status, l.Reason, l.ProjectID, l.UserID = domain.LinkQueued, "", project, user
	if err := s.update(ctx, &l); err != nil {
		return domain.TicketLink{}, err
	}
	if err := s.fill(ctx, l.AccountID, project); err != nil {
		return domain.TicketLink{}, err
	}
	return s.cfg.Store.TicketLinks().Get(ctx, l.AccountID, l.ID)
}

// Dismiss turns down a waiting ticket as by; the caller has checked by's
// permission.
func (s *Service) Dismiss(ctx context.Context, by domain.Actor, id uuid.UUID, reason string) (domain.TicketLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, _, _, err := s.waiting(ctx, by.AccountID, id)
	if err != nil {
		return domain.TicketLink{}, err
	}
	if err := s.settleGate(ctx, by, &l, domain.GateRejected, reason); err != nil {
		return domain.TicketLink{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		l.Status, l.Reason = domain.LinkDismissed, ""
		if err := tx.TicketLinks().Update(ctx, &l); err != nil {
			return err
		}
		return audit(ctx, tx, by, l, "ticket.dismissed", s.cfg.Now(), map[string]any{"key": l.Key})
	})
	return l, err
}

// settleGate resolves the pending intake gate of l as by.
func (s *Service) settleGate(ctx context.Context, by domain.Actor, l *domain.TicketLink, decision domain.GateStatus, feedback string) error {
	if l.GateID == uuid.Nil() {
		return nil
	}
	g, err := s.cfg.Gates.Get(ctx, l.AccountID, l.GateID)
	if err != nil {
		return fmt.Errorf("get the gate of %s: %w", l.Key, err)
	}
	if g.Status != domain.GatePending {
		return nil
	}
	if _, err := s.cfg.Gates.Resolve(ctx, by, g.ID, decision, feedback); err != nil {
		return fmt.Errorf("resolve the gate of %s: %w", l.Key, err)
	}
	return nil
}

// waiting returns a ticket of the Inbox with its connection and adapter.
func (s *Service) waiting(ctx context.Context, accountID, id uuid.UUID) (domain.TicketLink, domain.TrackerConnection, Adapter, error) {
	l, err := s.cfg.Store.TicketLinks().Get(ctx, accountID, id)
	if err != nil {
		return l, domain.TrackerConnection{}, nil, err
	}
	if l.Status != domain.LinkWaiting {
		return l, domain.TrackerConnection{}, nil, fmt.Errorf("ticket %s is %s: %w", l.Key, l.Status, ErrNotWaiting)
	}
	c, err := s.cfg.Store.TrackerConnections().Get(ctx, accountID, l.ConnectionID)
	if err != nil {
		return l, c, nil, fmt.Errorf("get connection: %w", err)
	}
	ad, err := s.cfg.Registry.Build(c)
	return l, c, ad, err
}
