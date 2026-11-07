// Package signintest is the rule that a sign-in connection is turned on only
// after a test sign-in worked (identity.md, Connections). A worked test is an
// audit entry that names what it vouches for: the kind, issuer, client ID and
// client secret reference the provider answered to. A connection is tested
// only while those are still what the newest entry names, so changing any of
// them, through any path, takes the test away, and a later test is needed.
package signintest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

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

// Record audits that a test sign-in of c worked, as by.
func Record(ctx context.Context, st store.Store, by domain.Actor, c domain.Connection) error {
	return append1(ctx, st, by, ActionTested, c, data{Fingerprint: Fingerprint(c)})
}

// Clear audits that the test of c no longer holds, because what it vouches
// for changed. Without it, changing back to the tested values would bring the
// old test back. It does nothing for a connection that was never tested.
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
// issuer and client ID.
func Valid(ctx context.Context, st store.Store, c domain.Connection) (bool, error) {
	m, err := Tested(ctx, st, c.AccountID, []domain.Connection{c})
	return m[c.ID], err
}

// Tested says for each of conns, all of the organization accountID, whether
// it has such a test.
func Tested(ctx context.Context, st store.Store, accountID uuid.UUID, conns []domain.Connection) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(conns) == 0 {
		return out, nil
	}
	entries, err := st.Audit().List(ctx, accountID, store.AuditFilter{SubjectType: "connection"})
	if err != nil {
		return nil, err
	}
	want := map[uuid.UUID]string{}
	for _, c := range conns {
		want[c.ID] = Fingerprint(c)
	}
	seen := map[uuid.UUID]bool{}
	for _, e := range entries { // newest first: the first of either action decides
		fp, ok := want[e.SubjectID]
		if !ok || seen[e.SubjectID] || (e.Action != ActionTested && e.Action != ActionCleared) {
			continue
		}
		seen[e.SubjectID] = true
		if e.Action != ActionTested {
			continue
		}
		var d data
		_ = json.Unmarshal(e.Data, &d) // an entry from before fingerprints vouches for nothing
		out[e.SubjectID] = d.Fingerprint == fp
	}
	return out, nil
}
