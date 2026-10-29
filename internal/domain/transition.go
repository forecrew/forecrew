package domain

import (
	"fmt"
	"slices"
)

// transitions maps a state to the states it may move to.
type transitions[S ~string] map[S][]S

func (t transitions[S]) can(from, to S) bool {
	return slices.Contains(t[from], to)
}

// apply returns to if the move is allowed and wraps ErrInvalidTransition otherwise.
func (t transitions[S]) apply(entity string, from, to S) (S, error) {
	if !t.can(from, to) {
		return from, invalidTransition(entity, from, to)
	}
	return to, nil
}

func invalidTransition[S ~string](entity string, from, to S) error {
	return fmt.Errorf("%s %q -> %q: %w", entity, from, to, ErrInvalidTransition)
}
