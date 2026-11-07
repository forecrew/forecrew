package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Token prefixes (docs/contracts/conventions.md).
const (
	prefixBootstrapToken = "fcb_"
	prefixWorkerToken    = "fcw_"
	prefixProviderToken  = "fcp_"
)

// newToken returns a random token and its hash.
func newToken(prefix string) (token string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(token))
	return token, h[:]
}

// JobsPerMachine is how many jobs a machine of s takes in its life: the
// shape's max_jobs, but one on an instance-wide shape. A hosted machine serves
// paying organizations, so nothing of one job, on disk or in memory, is ever
// left for the next: every job gets a fresh machine.
func JobsPerMachine(s domain.Shape) int {
	if s.Ownership() == domain.OwnershipHosted {
		return 1
	}
	return s.MaxJobs
}

// RecordLease records on its machine that the ephemeral worker w leased j,
// in the lease's own transaction tx. A machine that took its last job or
// stopped taking jobs since the query looked gives store.ErrConflict: the
// caller rolls the lease back and the job stays queued.
func RecordLease(ctx context.Context, tx store.Store, w domain.Worker, j domain.Job) error {
	m, err := tx.Machines().GetByWorker(ctx, w.AccountID, w.ID)
	if err != nil {
		return fmt.Errorf("machine of worker %s: %w", w.Name, err)
	}
	deadline := j.LeaseExpiresAt.Add(time.Duration(j.Spec.Timeout))
	m, err = tx.Machines().TakeJob(ctx, m.ID, j.ID, deadline)
	if err != nil {
		return err
	}
	shape, err := tx.Shapes().Get(ctx, m.ShapeID)
	if err != nil {
		return fmt.Errorf("shape of machine %s: %w", m.ID, err)
	}
	if m.JobsTaken > JobsPerMachine(shape) {
		return fmt.Errorf("machine %s took its jobs: %w", m.ID, store.ErrConflict)
	}
	switch m.State {
	case domain.MachineBusy:
		return nil
	case domain.MachineReady:
		return tx.Machines().UpdateState(ctx, m.ID, domain.MachineReady, domain.MachineBusy, "")
	}
	return fmt.Errorf("machine %s is %s: %w", m.ID, m.State, store.ErrConflict)
}

// ErrBootstrapRefused answers a bootstrap token that gets no worker: unknown,
// used, expired, or of a machine that can no longer take one.
var ErrBootstrapRefused = errors.New("bootstrap token refused")

// Grant is what a worker gets for its bootstrap token. Wait means ask again;
// the rest is empty then. WorkerToken is a secret.
type Grant struct {
	Wait        bool
	WorkerToken string
	Name        string
	Labels      []string
	MaxJobs     int
}

// Exchange answers a worker's first call (worker-api.md, POST
// /worker/v1/bootstrap) for the token with hash: it waits, or the machine gets
// its worker and is ready, all in one transaction.
func Exchange(ctx context.Context, st store.Store, hash []byte, version string) (Grant, error) {
	t, err := st.BootstrapTokens().GetByTokenHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return Grant{}, ErrBootstrapRefused
	}
	if err != nil {
		return Grant{}, fmt.Errorf("find bootstrap token: %w", err)
	}
	m, err := st.Machines().Get(ctx, t.MachineID)
	if err != nil {
		return Grant{}, fmt.Errorf("machine of bootstrap token: %w", err)
	}
	switch {
	case m.State == domain.MachineRequested && t.UsedAt.IsZero():
		return Grant{Wait: true}, nil
	case m.State == domain.MachineBooting && !t.Assigned():
		if err := st.Machines().MarkUp(ctx, m.ID); err != nil {
			return Grant{}, fmt.Errorf("mark machine up: %w", err)
		}
		return Grant{Wait: true}, nil
	case m.State != domain.MachineBooting:
		return Grant{}, ErrBootstrapRefused
	}
	var g Grant
	err = st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if _, err := tx.BootstrapTokens().Consume(ctx, hash); err != nil {
			return err
		}
		shape, err := tx.Shapes().Get(ctx, m.ShapeID)
		if err != nil {
			return fmt.Errorf("shape of machine: %w", err)
		}
		token, tokenHash := newToken(prefixWorkerToken)
		w := domain.Worker{
			AccountID: m.AccountID, Name: workerName(m), Pool: shape.Pool, Ephemeral: true, TokenHash: tokenHash,
			Labels: shape.OfferedLabels(), Version: version, MaxJobs: JobsPerMachine(shape),
		}
		if err := tx.Workers().Create(ctx, &w); err != nil {
			return fmt.Errorf("create worker: %w", err)
		}
		if err := tx.Machines().SetWorker(ctx, m.ID, w.ID); err != nil {
			return fmt.Errorf("link worker: %w", err)
		}
		if err := tx.Machines().UpdateState(ctx, m.ID, domain.MachineBooting, domain.MachineReady, ""); err != nil {
			return err
		}
		g = Grant{WorkerToken: token, Name: w.Name, Labels: w.Labels, MaxJobs: w.MaxJobs}
		return nil
	})
	if errors.Is(err, store.ErrConflict) {
		return Grant{}, ErrBootstrapRefused
	}
	return g, err
}

// workerName names a machine's worker from the random end of its ID; the
// start of a UUIDv7 is a timestamp, the same for machines of one minute.
func workerName(m domain.Machine) string { return "machine-" + idTail(m.ID, 12) }

// idTail returns the last n characters of id.
func idTail(id uuid.UUID, n int) string {
	s := id.String()
	return s[len(s)-n:]
}

// OrgInstance in place of an organization's slug asks for an instance-wide
// provider. An organization with this slug cannot be named by it here.
const OrgInstance = "instance"

// ErrOrgRequired means a server with several organizations was not told whose
// a new provider is.
var ErrOrgRequired = errors.New(`several organizations: name one, or "` + OrgInstance + `" for an instance-wide location`)

// ProviderOrganization says whose a new provider is (fleet.md, Ownership): the
// organization of slug, instance-wide (the zero ID) for OrgInstance or, with no
// slug, the server's only organization, so a home server is the organization's
// own. On a server with several no slug is ErrOrgRequired: nothing instance-wide
// happens by accident.
func ProviderOrganization(ctx context.Context, st store.Store, slug string) (uuid.UUID, error) {
	if slug == OrgInstance {
		return uuid.Nil(), nil
	}
	accounts, err := st.Accounts().List(ctx)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("list organizations: %w", err)
	}
	if slug == "" {
		if len(accounts) == 1 {
			return accounts[0].ID, nil
		}
		return uuid.Nil(), ErrOrgRequired
	}
	for _, a := range accounts {
		if a.Slug == slug {
			return a.ID, nil
		}
	}
	return uuid.Nil(), fmt.Errorf("organization %s: %w", slug, store.ErrNotFound)
}
