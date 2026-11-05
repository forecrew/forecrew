package councils

import (
	"fmt"
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
)

// adhocBase is the pack an adhoc council follows: it has no pack of its
// own, and this one is the widest.
const adhocBase = domain.CouncilTechnical

// adhocQuestion is what an adhoc council answers, in general terms.
const adhocQuestion = "The question a human put to the council."

// Seated returns the pack a council of kind runs with. An adhoc council
// follows the technical pack. With roles, those are the members instead of
// the kind's, each with its seat in the pack or else a seat of its own, and
// the devil's advocate sits whether it is named or not. A role a council
// cannot seat wraps ErrInvalidTurn.
func (s *Set) Seated(kind domain.CouncilKind, roles []string) (Pack, error) {
	base := kind
	if kind == domain.CouncilAdhoc {
		base = adhocBase
	}
	p, err := s.Pack(base)
	if err != nil {
		return Pack{}, err
	}
	if kind == domain.CouncilAdhoc {
		p.Kind, p.Question = kind, adhocQuestion
	}
	if len(roles) == 0 {
		return p, nil
	}
	members := make([]Seat, 0, len(roles)+1)
	for i, role := range roles {
		if _, err := s.role(role); err != nil {
			return Pack{}, fmt.Errorf("%w: %w", ErrInvalidTurn, err)
		}
		if role == p.Facilitator {
			return Pack{}, fmt.Errorf("%w: the facilitator %q cannot be a member", ErrInvalidTurn, role)
		}
		if slices.Contains(roles[:i], role) {
			return Pack{}, fmt.Errorf("%w: role %q is listed twice", ErrInvalidTurn, role)
		}
		members = append(members, p.seatOf(role, i))
	}
	if !slices.Contains(roles, DevilsAdvocate) {
		members = append(members, p.seatOf(DevilsAdvocate, len(roles)))
	}
	if len(members) < minMembers {
		return Pack{}, fmt.Errorf("%w: a council needs at least %d members, got %d", ErrInvalidTurn, minMembers, len(members))
	}
	p.Members = members
	return p, nil
}

// seatOf returns the seat of role in p, or else a seat with the role's name
// as its priority. The i-th such seat prefers the i-th adapter, so that
// neighbours start on different vendors like the seats of a pack.
func (p Pack) seatOf(role string, i int) Seat {
	if j := slices.IndexFunc(p.Members, func(m Seat) bool { return m.Role == role }); j >= 0 {
		return p.Members[j]
	}
	first := i % len(newSeatAdapters)
	return Seat{
		Role: role, Priority: role, Focus: "the question as the " + role + " sees it",
		Runtimes: slices.Concat(newSeatAdapters[first:], newSeatAdapters[:first]),
	}
}

// Member is a seat pinned to the runtime its jobs run on.
type Member struct {
	Role     string `json:"role"`
	Priority string `json:"priority"`
	Runtime  string `json:"runtime"`
}

// Assign pins the members of p to runtimes: each takes the first runtime it
// prefers that offered accepts, or its first one when none is, so its jobs
// wait for a worker. A seat without runtimes takes usual(role). When that
// leaves everyone on one runtime although another is offered to someone,
// the last such seat moves there, so that two vendors take part whenever
// the workers offer them.
func (p Pack) Assign(offered func(s Seat, runtime string) bool, usual func(role string) string) []Member {
	out := make([]Member, len(p.Members))
	seen := map[string]bool{}
	for i, s := range p.Members {
		choices := s.Runtimes
		if len(choices) == 0 {
			choices = []string{usual(s.Role)}
		}
		out[i] = Member{Role: s.Role, Priority: s.Priority, Runtime: choices[0]}
		if j := slices.IndexFunc(choices, func(r string) bool { return offered(s, r) }); j >= 0 {
			out[i].Runtime = choices[j]
		}
		seen[out[i].Runtime] = true
	}
	if len(seen) > 1 {
		return out
	}
	for i, s := range slices.Backward(p.Members) {
		j := slices.IndexFunc(s.Runtimes, func(r string) bool { return !seen[r] && offered(s, r) })
		if j >= 0 {
			out[i].Runtime = s.Runtimes[j]
			break
		}
	}
	return out
}
