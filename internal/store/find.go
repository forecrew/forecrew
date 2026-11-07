package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
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

// EnforcedConnections returns the connections an organization requires of
// the user: those of the verified domains it enforces that hold the user's
// primary email or the email of one of their identities. Empty means the user
// signs in as they like (docs/contracts/identity.md, verified_domains).
//
// Only the enterprise build turns enforcement on, but every build reads it,
// as sign-in discovery does, so a move to the open-core build lifts nothing.
func EnforcedConnections(ctx context.Context, st Store, accountID, userID uuid.UUID) ([]uuid.UUID, error) {
	user, err := st.Users().Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	identities, err := st.Identities().ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	names := []string{emailDomain(user.Email)}
	for _, i := range identities {
		names = append(names, emailDomain(i.Email))
	}
	slices.Sort(names)
	var out []uuid.UUID
	for _, name := range slices.Compact(names) {
		if name == "" {
			continue
		}
		d, err := st.VerifiedDomains().GetVerified(ctx, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if d.EnforceSSO && d.AccountID == accountID {
			out = append(out, d.ConnectionID)
		}
	}
	return out, nil
}

// emailDomain returns the domain of an email address, lowercase; empty
// without one.
func emailDomain(email string) string {
	_, name, ok := strings.Cut(strings.ToLower(email), "@")
	if !ok {
		return ""
	}
	return name
}

// PersonalTokenCap returns how long the user's personal tokens may last in
// the organization: its max_token_ttl under SSO enforcement, zero for no cap.
func PersonalTokenCap(ctx context.Context, st Store, accountID, userID uuid.UUID) (time.Duration, error) {
	enforced, err := EnforcedConnections(ctx, st, accountID, userID)
	if err != nil || len(enforced) == 0 {
		return 0, err
	}
	a, err := st.Accounts().Get(ctx, accountID)
	return time.Duration(a.MaxTokenTTL), err
}
