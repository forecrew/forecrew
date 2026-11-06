package store

import (
	"fmt"
	"strconv"
	"uuid"
)

// FindActive returns the first item that is not revoked and whose name or ID
// is key; it wraps ErrNotFound when there is none. info reports an item's ID,
// name and whether it is revoked. Tokens and workers are revoked by name or ID.
func FindActive[T any](key string, items []T, info func(T) (id uuid.UUID, name string, revoked bool)) (T, error) {
	for _, it := range items {
		id, name, revoked := info(it)
		if !revoked && (name == key || id.String() == key) {
			return it, nil
		}
	}
	var zero T
	return zero, fmt.Errorf("%q: %w", key, ErrNotFound)
}

// FreeTokenName returns name, or the first free name-2, name-3, ... when
// used holds it (PersonalTokenRepo.AssignOwner).
func FreeTokenName(name string, used map[string]bool) string {
	free := name
	for n := 2; used[free]; n++ {
		free = name + "-" + strconv.Itoa(n)
	}
	return free
}
