package tracker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
)

// Backoff of the outbox.
const (
	firstRetry = 30 * time.Second
	lastRetry  = time.Hour
)

// say queues a comment on a ticket, once for its key.
func (s *Service) say(ctx context.Context, st store.Store, l domain.TicketLink, key, text string) error {
	return s.enqueue(ctx, st, l, "comment:"+key, domain.WriteComment, text)
}

func (s *Service) enqueue(ctx context.Context, st store.Store, l domain.TicketLink, key string, kind domain.TrackerWriteKind, value string) error {
	_, err := st.TrackerOutbox().Enqueue(ctx, &domain.TrackerWrite{
		AccountID: l.AccountID, ConnectionID: l.ConnectionID, LinkID: l.ID, Key: l.Key, DedupeKey: key, Kind: kind, Value: value, NextAttempt: s.cfg.Now(),
	})
	if err != nil {
		return fmt.Errorf("queue a write to %s: %w", l.Key, err)
	}
	return nil
}

// setState records a logical state and queues the transition its route maps
// it to, if any.
func (s *Service) setState(ctx context.Context, st store.Store, l *domain.TicketLink, state domain.TicketState) error {
	l.State = state
	if err := st.TicketLinks().Update(ctx, l); err != nil {
		return fmt.Errorf("update the link of %s: %w", l.Key, err)
	}
	if l.RouteID == uuid.Nil() {
		return nil
	}
	r, err := st.TrackerRoutes().Get(ctx, l.AccountID, l.RouteID)
	if err != nil {
		return fmt.Errorf("get route: %w", err)
	}
	target, ok := r.States[state]
	if !ok {
		return nil
	}
	return s.enqueue(ctx, st, *l, "state:"+domain.NewID().String(), domain.WriteTransition, target)
}

// Sync brings every live ticket up to date: it acts on decided intake gates,
// reports the state of goals, starts what the limits allow and drains the
// outbox. Everything it does is read from the database, so a restart loses
// nothing.
func (s *Service) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := s.cfg.Store.Accounts().List(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	var errs []error
	for _, a := range accounts {
		errs = append(errs, s.syncAccount(ctx, a.ID), s.drain(ctx, a.ID))
	}
	return errors.Join(errs...)
}

func (s *Service) syncAccount(ctx context.Context, accountID uuid.UUID) error {
	links, err := s.cfg.Store.TicketLinks().List(ctx, accountID, store.TicketLinkFilter{Live: true})
	if err != nil {
		return fmt.Errorf("list tickets: %w", err)
	}
	var errs []error
	projects := map[uuid.UUID]bool{}
	for _, l := range links {
		switch l.Status {
		case domain.LinkWaiting:
			errs = append(errs, s.settleWaiting(ctx, l))
		case domain.LinkActive:
			errs = append(errs, s.report(ctx, l))
		}
		if l.ProjectID != uuid.Nil() {
			projects[l.ProjectID] = true
		}
	}
	for p := range projects {
		errs = append(errs, s.fill(ctx, accountID, p))
	}
	return errors.Join(errs...)
}

// settleWaiting acts on the decision of a waiting ticket's gate, which anyone
// with its permission can take from any entry point.
func (s *Service) settleWaiting(ctx context.Context, l domain.TicketLink) error {
	if l.GateID == uuid.Nil() {
		return nil
	}
	g, err := s.cfg.Gates.Get(ctx, l.AccountID, l.GateID)
	if err != nil {
		return fmt.Errorf("get the gate of %s: %w", l.Key, err)
	}
	switch g.Status {
	case domain.GateApproved:
		l.Status, l.Reason, l.UserID = domain.LinkQueued, "", uuid.Nil()
		if by, err := domain.ParseActor(g.ResolvedBy); err == nil && by.Kind == domain.ActorUser {
			if id, ok := by.SubjectID(); ok {
				l.UserID = id
			}
		}
		return s.update(ctx, &l)
	case domain.GateRejected:
		return s.end(ctx, &l, domain.LinkDismissed)
	}
	return nil
}

// logical is the state a goal gives its ticket.
func logical(g domain.Goal, tasks []domain.Task) domain.TicketState {
	switch g.Status {
	case domain.GoalDone:
		return domain.TicketDone
	case domain.GoalCanceled:
		return domain.TicketCanceled
	}
	if slices.ContainsFunc(tasks, func(t domain.Task) bool {
		return t.Status == domain.TaskInReview || t.Status == domain.TaskMergeReady
	}) {
		return domain.TicketInReview
	}
	return domain.TicketInProgress
}

// report sends what happened to the goal of an active ticket back to it.
func (s *Service) report(ctx context.Context, l domain.TicketLink) error {
	st := s.cfg.Store
	g, err := st.Goals().Get(ctx, l.AccountID, l.GoalID)
	if err != nil {
		return fmt.Errorf("get the goal of %s: %w", l.Key, err)
	}
	if g.Status == domain.GoalDraft {
		if err := s.restart(ctx, l, g); err != nil {
			return err
		}
	}
	tasks, err := st.Tasks().List(ctx, l.AccountID, store.TaskFilter{GoalID: g.ID})
	if err != nil {
		return fmt.Errorf("list the tasks of %s: %w", g.Ref(), err)
	}
	notes, err := s.notes(ctx, l, g, tasks)
	if err != nil {
		return err
	}
	return st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		for key, text := range notes {
			if err := s.say(ctx, tx, l, key, text); err != nil {
				return err
			}
		}
		state := logical(g, tasks)
		if state != l.State {
			if err := s.setState(ctx, tx, &l, state); err != nil {
				return err
			}
		}
		switch g.Status {
		case domain.GoalDone:
			l.Status = domain.LinkDone
		case domain.GoalCanceled:
			l.Status = domain.LinkCanceled
		default:
			return nil
		}
		return tx.TicketLinks().Update(ctx, &l)
	})
}

// restart starts the run of a goal that has none, after a crash; the run's ID
// makes this a no-op otherwise.
func (s *Service) restart(ctx context.Context, l domain.TicketLink, g domain.Goal) error {
	p, err := s.cfg.Store.Projects().Get(ctx, l.AccountID, g.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	_, playbook, err := s.goalPlan(ctx, l, g.Kind, p)
	if err != nil {
		return err
	}
	return s.startRun(ctx, l, g, p, playbook)
}

// notes are the comments a goal owes its ticket, by dedupe key: its pull
// requests, the questions of its agents and its outcome.
func (s *Service) notes(ctx context.Context, l domain.TicketLink, g domain.Goal, tasks []domain.Task) (map[string]string, error) {
	st := s.cfg.Store
	out := map[string]string{}
	for _, t := range tasks {
		changes, err := st.Changes().List(ctx, l.AccountID, store.ChangeFilter{TaskID: t.ID})
		if err != nil {
			return nil, fmt.Errorf("list the pull requests of %s: %w", t.Ref(), err)
		}
		for _, c := range changes {
			out[fmt.Sprintf("pr:%d", c.Number)] = fmt.Sprintf("Pull request for %s: %s", g.Ref(), c.URL)
		}
	}
	questions, err := s.questions(ctx, l.AccountID, g)
	if err != nil {
		return nil, err
	}
	for _, q := range questions {
		text := q.Summary
		if i := strings.Index(text, "<untrusted-data"); i >= 0 {
			text = text[i:] // from the marked question on: the rest is for the Forecrew UI
		}
		out["gate:"+q.Ref()] = fmt.Sprintf("%s has a question. Answer with `@forecrew reply <your answer>` or in Forecrew: %s\n\n%s", g.Ref(), s.goalURL(g), text)
	}
	switch g.Status {
	case domain.GoalDone:
		out["outcome"] = fmt.Sprintf("%s is done: %s", g.Ref(), s.goalURL(g))
	case domain.GoalCanceled:
		out["outcome"] = fmt.Sprintf("%s was canceled. Pull requests that were merged stay merged: %s", g.Ref(), s.goalURL(g))
	}
	return out, nil
}

// questions returns the pending ask_human questions of a goal, oldest first.
func (s *Service) questions(ctx context.Context, accountID uuid.UUID, g domain.Goal) ([]domain.Gate, error) {
	pending, err := s.cfg.Store.Gates().List(ctx, accountID, store.GateFilter{ProjectID: g.ProjectID, Kind: domain.GateNeedsYou, Status: domain.GatePending})
	if err != nil {
		return nil, fmt.Errorf("list the questions of %s: %w", g.Ref(), err)
	}
	return slices.DeleteFunc(pending, func(x domain.Gate) bool { return !x.AsksHuman() || x.GoalID != g.ID }), nil
}

// follow looks at the ticket of a goal at work. A person's comments and
// changes count here, and nothing in them changes the goal by itself.
func (s *Service) follow(ctx context.Context, c domain.TrackerConnection, ad Adapter, l domain.TicketLink, t Ticket) error {
	g, err := s.cfg.Store.Goals().Get(ctx, l.AccountID, l.GoalID)
	if err != nil {
		return fmt.Errorf("get the goal of %s: %w", l.Key, err)
	}
	if g.Status.Terminal() {
		return nil // Sync closes the link
	}
	if err := s.readComments(ctx, c, ad, &l, g, t); err != nil {
		return err
	}
	if t.Closed {
		return s.closedByPerson(ctx, l, g)
	}
	if hash := contentHash(t); hash != l.ContentHash {
		return s.possibleChange(ctx, l, g, t, hash)
	}
	return nil
}

// closedByPerson cancels the goal of a closed ticket, unless a merge is under
// way.
func (s *Service) closedByPerson(ctx context.Context, l domain.TicketLink, g domain.Goal) error {
	merging, err := s.merging(ctx, g)
	if err != nil {
		return err
	}
	if merging {
		return s.say(ctx, s.cfg.Store, l, "closed-merging", fmt.Sprintf("This ticket was closed while %s is merging: the merge goes on, and the goal finishes.", g.Ref()))
	}
	if err := s.cfg.Runs.CancelRun(ctx, l.AccountID, l.ID, "The ticket "+l.Key+" was closed."); err != nil {
		return fmt.Errorf("cancel %s: %w", g.Ref(), err)
	}
	return audit(ctx, s.cfg.Store, s.forecrewd(l.AccountID), l, "ticket.closed", s.cfg.Now(), map[string]any{"key": l.Key, "goal": g.Ref()})
}

// merging reports whether a merge of the goal is under way: one was approved
// and has not landed, or every task is merged and the goal is closing.
func (s *Service) merging(ctx context.Context, g domain.Goal) (bool, error) {
	tasks, err := s.cfg.Store.Tasks().List(ctx, g.AccountID, store.TaskFilter{GoalID: g.ID})
	if err != nil {
		return false, fmt.Errorf("list the tasks of %s: %w", g.Ref(), err)
	}
	if len(tasks) > 0 && !slices.ContainsFunc(tasks, func(t domain.Task) bool { return t.Status != domain.TaskMerged }) {
		return true, nil
	}
	approved, err := s.cfg.Store.Gates().List(ctx, g.AccountID, store.GateFilter{ProjectID: g.ProjectID, Kind: domain.GateMergeApproval, Status: domain.GateApproved})
	if err != nil {
		return false, fmt.Errorf("list the merge gates of %s: %w", g.Ref(), err)
	}
	return slices.ContainsFunc(tasks, func(t domain.Task) bool {
		return t.Status == domain.TaskMergeReady && slices.ContainsFunc(approved, func(x domain.Gate) bool { return x.TaskID == t.ID })
	}), nil
}

// possibleChange shows an edit of the ticket in the Inbox. The goal keeps the
// text it started from; one gate waits at a time.
func (s *Service) possibleChange(ctx context.Context, l domain.TicketLink, g domain.Goal, t Ticket, hash string) error {
	l.ContentHash, l.Title = hash, t.Title
	if l.ChangeGateID != uuid.Nil() {
		open, err := s.cfg.Gates.Get(ctx, l.AccountID, l.ChangeGateID)
		if err != nil {
			return fmt.Errorf("get the change gate of %s: %w", l.Key, err)
		}
		if open.Status == domain.GatePending {
			return s.update(ctx, &l)
		}
	}
	p, err := s.cfg.Store.Projects().Get(ctx, l.AccountID, g.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	_, err = s.cfg.Gates.Open(ctx, gates.OpenRequest{
		AccountID: l.AccountID, ProjectID: g.ProjectID, Kind: domain.GateTicketIntake, SubjectRepo: p.Repo, GoalID: g.ID,
		Title:   "Ticket " + l.Key + " changed while " + g.Ref() + " works on it",
		Summary: "This is a possible change: " + g.Ref() + " keeps the text it started from, whatever you decide here. Change the goal yourself if it should follow. " + t.URL + "\n\n" + ticketText(t),
		Opened: func(ctx context.Context, tx store.Store, gate domain.Gate) error {
			l.ChangeGateID = gate.ID
			return tx.TicketLinks().Update(ctx, &l)
		},
	})
	if err != nil {
		return fmt.Errorf("open the change gate of %s: %w", l.Key, err)
	}
	return nil
}

var replyRE = regexp.MustCompile(`(?is)(?:^|\n)[ \t]*@forecrew[ \t]+reply\b[ \t]*(.+)`)

// parseReply returns the answer of a comment that is a reply command.
func parseReply(body string) (string, bool) {
	m := replyRE.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	text := strings.TrimSpace(m[1])
	return text, text != ""
}

// unread returns the comments after the cursor. A cursor that is gone falls
// back on the time the link was made.
func unread(l domain.TicketLink, t Ticket) []Comment {
	if l.CommentCursor == "" && len(t.Comments) > 0 {
		return t.Comments
	}
	i := slices.IndexFunc(t.Comments, func(c Comment) bool { return c.ID == l.CommentCursor })
	if i >= 0 {
		return t.Comments[i+1:]
	}
	return slices.DeleteFunc(slices.Clone(t.Comments), func(c Comment) bool { return !c.At.After(l.CreatedAt) })
}

// readComments acts on the new comments of a ticket. The only one that does
// anything is a reply: approvals stay in Forecrew.
func (s *Service) readComments(ctx context.Context, c domain.TrackerConnection, ad Adapter, l *domain.TicketLink, g domain.Goal, t Ticket) error {
	fresh := unread(*l, t)
	if len(fresh) == 0 {
		return nil
	}
	for _, cm := range fresh {
		own, err := s.cfg.Store.TrackerOutbox().Sent(ctx, l.AccountID, l.ID, cm.ID)
		if err != nil {
			return fmt.Errorf("check comment %s: %w", cm.ID, err)
		}
		if own || cm.Author.Bot {
			continue
		}
		if text, ok := parseReply(cm.Body); ok {
			if err := s.reply(ctx, c, ad, *l, g, cm, text); err != nil {
				return err
			}
		}
	}
	l.CommentCursor = lastComment(t)
	return s.update(ctx, l)
}

// reply answers the oldest question of the goal as the mapped user. It
// resolves only ask_human gates, which no playbook step waits on; the answer
// is ticket text, so it reaches the agent marked.
func (s *Service) reply(ctx context.Context, c domain.TrackerConnection, ad Adapter, l domain.TicketLink, g domain.Goal, cm Comment, text string) error {
	user, ok, err := s.person(ctx, c, ad, cm.Author)
	if err != nil {
		return err
	}
	if !ok {
		s.cfg.Logger.Warn("reply from an unknown tracker user", "account_id", l.AccountID, "connection", c.ID, "key", l.Key)
		return audit(ctx, s.cfg.Store, s.forecrewd(l.AccountID), l, "ticket.reply_ignored", s.cfg.Now(), map[string]any{"key": l.Key, "comment": cm.ID})
	}
	questions, err := s.questions(ctx, l.AccountID, g)
	if err != nil {
		return err
	}
	key := "reply:" + cm.ID
	if len(questions) == 0 {
		return s.say(ctx, s.cfg.Store, l, key, "No question is waiting for an answer here.")
	}
	q := questions[0]
	by := domain.UserActor(l.AccountID, user, domain.Credential{})
	switch err := s.cfg.Authz.AuthorizeGate(ctx, by, q); {
	case errors.Is(err, authz.ErrDenied):
		return s.say(ctx, s.cfg.Store, l, key, fmt.Sprintf("You need the permission `%s` on this project to answer.", domain.PermGateApproveNeedsYou))
	case err != nil:
		return err
	}
	answer := contextpack.WrapUntrusted("reply on ticket "+l.Key, text)
	if _, err := s.cfg.Gates.Resolve(ctx, by, q.ID, domain.GateApproved, answer); err != nil {
		return fmt.Errorf("answer %s: %w", q.Ref(), err)
	}
	return s.say(ctx, s.cfg.Store, l, key, q.Ref()+" is answered.")
}

// retryAfter is the wait after a failed attempt.
func retryAfter(attempts int) time.Duration {
	if attempts >= 7 {
		return lastRetry
	}
	return min(firstRetry<<attempts, lastRetry)
}

// drain sends the writes that are due. A failure waits for its retry and
// touches nothing else: a tracker outage never stops a goal.
func (s *Service) drain(ctx context.Context, accountID uuid.UUID) error {
	box := s.cfg.Store.TrackerOutbox()
	due, err := box.Due(ctx, accountID, s.cfg.Now(), dueBatch)
	if err != nil {
		return fmt.Errorf("list due writes: %w", err)
	}
	var errs []error
	for _, w := range due {
		id, err := s.send(ctx, w)
		switch {
		case err == nil, errors.Is(err, ErrNotFound): // a ticket that is gone takes no more writes
			errs = append(errs, box.MarkSent(ctx, accountID, w.ID, id, s.cfg.Now()))
		default:
			s.cfg.Logger.Warn("write to tracker", "account_id", accountID, "connection", w.ConnectionID, "key", w.Key, "attempts", w.Attempts+1, "error", err)
			errs = append(errs, box.MarkFailed(ctx, accountID, w.ID, clip(err.Error(), 500), s.cfg.Now().Add(retryAfter(w.Attempts))))
		}
	}
	return errors.Join(errs...)
}

// send does one write, leaving out what the tracker cannot do.
func (s *Service) send(ctx context.Context, w domain.TrackerWrite) (string, error) {
	c, err := s.cfg.Store.TrackerConnections().Get(ctx, w.AccountID, w.ConnectionID)
	if err != nil {
		return "", err
	}
	ad, err := s.cfg.Registry.Build(c)
	if err != nil {
		return "", err
	}
	caps := ad.Capabilities()
	switch {
	case w.Kind == domain.WriteComment && caps.Comments:
		return ad.Comment(ctx, w.Key, w.Value)
	case w.Kind == domain.WriteTransition && caps.Transitions:
		return "", ad.Transition(ctx, w.Key, w.Value)
	}
	return "", nil
}
