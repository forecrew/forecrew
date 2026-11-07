// Package signintest is the rule that a sign-in connection is turned on only
// after a test sign-in worked (contracts/identity.md, Connections: a test
// sign-in vouches for the connection's kind, issuer, client ID and secret
// reference). A worked test is an audit entry that names what it vouches for:
// those four fields, as a fingerprint. A connection is tested only while they
// are still what the newest entry names, so changing any of them, through any
// path including config plans, takes the test away, and a later test is
// needed. Whatever records or clears a test holds the lock of the connection
// row (store.ConnectionRepo.Lock), so the two cannot interleave.
package signintest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ErrChanged is Record's refusal: the connection changed during the test.
var ErrChanged = errors.New("the connection changed during the test; test again")

// Audit actions of a connection's tests.
const (
	ActionTested  = "connection.tested"
	ActionCleared = "connection.test_cleared"
)

// Fingerprint identifies what a test sign-in vouches for in c. The group
// claim is not in it: a test signs nobody in and reads no groups. A secret
// reference is in it, a rotated value behind the same reference is not.
func Fingerprint(c domain.Connection) string {
	h := sha256.New()
	for _, part := range []string{string(c.Kind), c.Issuer, c.ClientID, c.ClientSecretRef} {
		fmt.Fprintf(h, "%d:%s,", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type data struct {
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Record audits that a test sign-in of c worked, as by. It holds the lock of
// the connection's row and refuses (store.ErrConflict) when the row no longer
// has the values c was tested with, as a test of values that are gone vouches
// for nothing and must not come back when they do.
func Record(ctx context.Context, st store.Store, by domain.Actor, c domain.Connection) error {
	return st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Connections().Lock(ctx, c.ID); err != nil {
			return err
		}
		stored, err := tx.Connections().Get(ctx, c.ID)
		if err != nil {
			return err
		}
		if Fingerprint(stored) != Fingerprint(c) {
			return fmt.Errorf("%w: %w", ErrChanged, store.ErrConflict)
		}
		return append1(ctx, tx, by, ActionTested, c, data{Fingerprint: Fingerprint(c)})
	})
}

// Clear audits that the test of c no longer holds, because what it vouches
// for (kind, issuer, client ID, secret reference) changed. Without it,
// changing back to the tested values would bring the old test back. It does
// nothing for a connection that was never tested. The caller holds the lock
// of the connection's row.
func Clear(ctx context.Context, st store.Store, by domain.Actor, c domain.Connection) error {
	ok, err := Valid(ctx, st, c)
	if err != nil || !ok {
		return err
	}
	return append1(ctx, st, by, ActionCleared, c, data{})
}

func append1(ctx context.Context, st store.Store, by domain.Actor, action string, c domain.Connection, d data) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return st.Audit().Append(ctx, &domain.AuditEntry{AccountID: by.AccountID, Actor: by.String(), Credential: by.Credential.String(),
		Action: action, SubjectType: "connection", SubjectID: c.ID, Data: raw})
}

// Valid reports whether c has a test sign-in that worked for its current
// kind, issuer, client ID and secret reference.
func Valid(ctx context.Context, st store.Store, c domain.Connection) (bool, error) {
	m, err := Tested(ctx, st, c.AccountID, []domain.Connection{c})
	return m[c.ID], err
}

// Tested says for each of conns, all of the organization accountID, whether
// it has such a test.
func Tested(ctx context.Context, st store.Store, accountID uuid.UUID, conns []domain.Connection) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	for _, c := range conns {
		// By subject, so the index on it serves; newest first, the first
		// entry of either action decides.
		entries, err := st.Audit().List(ctx, accountID, store.AuditFilter{SubjectType: "connection", SubjectID: c.ID})
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Action != ActionTested && e.Action != ActionCleared {
				continue
			}
			if e.Action == ActionTested {
				var d data
				_ = json.Unmarshal(e.Data, &d) // an entry from before fingerprints vouches for nothing
				out[c.ID] = d.Fingerprint == Fingerprint(c)
			}
			break
		}
	}
	return out, nil
}
