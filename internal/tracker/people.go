package tracker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// maxUnmappedReads bounds how many waiting tickets of one connection are read
// from their tracker to find who handed them over.
const maxUnmappedReads = 25

// Unmapped is a person on a tracker who handed Forecrew a ticket and maps to
// no Forecrew user, with the keys of the tickets that wait for it.
type Unmapped struct {
	Connection domain.TrackerConnection
	User       User
	Tickets    []string
}

// MaxActiveGoals is trackers.max_active_goals, the limit of goals from tickets
// at work in a project.
func (s *Service) MaxActiveGoals() int { return s.cfg.MaxActiveGoals }

// Unmapped finds who handed over the tickets that wait in the Inbox because
// their person is unknown, so someone can map them. The waiting link keeps
// only the person's name, so each ticket is read from its tracker, within
// bounds. A connection that cannot be read is reported in the error and the
// others still answer. only limits it to one connection.
func (s *Service) Unmapped(ctx context.Context, accountID, only uuid.UUID) ([]Unmapped, error) {
	links, err := s.cfg.Store.TicketLinks().List(ctx, accountID, store.TicketLinkFilter{ConnectionID: only, Status: domain.LinkWaiting})
	if err != nil {
		return nil, err
	}
	byConn := map[uuid.UUID][]domain.TicketLink{}
	for _, l := range links {
		if l.Reason == domain.WaitUnknownPerson && l.ConnectionID != uuid.Nil() {
			byConn[l.ConnectionID] = append(byConn[l.ConnectionID], l)
		}
	}
	var out []Unmapped
	var errs []error
	for _, id := range slices.SortedFunc(mapKeys(byConn), func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) }) {
		found, err := s.unmappedOf(ctx, accountID, id, byConn[id][:min(len(byConn[id]), maxUnmappedReads)])
		out = append(out, found...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

func mapKeys[V any](m map[uuid.UUID]V) func(func(uuid.UUID) bool) {
	return func(yield func(uuid.UUID) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func (s *Service) unmappedOf(ctx context.Context, accountID, connection uuid.UUID, links []domain.TicketLink) ([]Unmapped, error) {
	c, err := s.cfg.Store.TrackerConnections().Get(ctx, accountID, connection)
	if err != nil {
		return nil, fmt.Errorf("get connection: %w", err)
	}
	ad, err := s.cfg.Registry.Build(c)
	if err != nil {
		return nil, fmt.Errorf("connection %s: %w", c.Name, err)
	}
	var out []Unmapped
	var errs []error
	for _, l := range links {
		t, err := s.read(ctx, c, ad, l.Key)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				errs = append(errs, fmt.Errorf("connection %s: %w", c.Name, err))
			}
			continue
		}
		h, ok := handover(c.Triggers, t)
		if !ok || h.By.ID == "" || h.By.Bot {
			continue
		}
		if _, mapped, err := s.person(ctx, c, ad, h.By); err != nil || mapped {
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		i := slices.IndexFunc(out, func(u Unmapped) bool { return u.User.ID == h.By.ID })
		if i < 0 {
			out = append(out, Unmapped{Connection: c, User: h.By})
			i = len(out) - 1
		}
		out[i].Tickets = append(out[i].Tickets, l.Key)
	}
	return out, errors.Join(errs...)
}
