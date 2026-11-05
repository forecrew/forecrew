//go:build integration

package postgres

import (
	"strconv"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// accessSeed seeds what sign-in creates, with plain SQL.
type accessSeed struct{ f *fixture }

func (s accessSeed) Member(t *testing.T, accountID uuid.UUID) uuid.UUID {
	t.Helper()
	id := domain.NewID()
	must(t, exec(s.f.pool, `INSERT INTO users (id, name) VALUES ($1, 'someone')`, id))
	must(t, exec(s.f.pool, `INSERT INTO memberships (account_id, user_id, status, source) VALUES ($1, $2, 'active', 'manual')`, accountID, id))
	return id
}

func (s accessSeed) SetStatus(t *testing.T, accountID, userID uuid.UUID, status domain.MembershipStatus) {
	t.Helper()
	must(t, exec(s.f.pool, `UPDATE memberships SET status = $3 WHERE account_id = $1 AND user_id = $2`, accountID, userID, string(status)))
}

func (s accessSeed) Link(t *testing.T, _, userID uuid.UUID, id int64, login string) {
	t.Helper()
	conn := domain.NewID()
	must(t, exec(s.f.pool, `INSERT INTO connections (id, kind, name, client_id, client_secret_ref) VALUES ($1, 'github', 'github', 'c', 'env:GITHUB_SECRET')`, conn))
	must(t, exec(s.f.pool, `INSERT INTO identities (user_id, connection_id, subject, login) VALUES ($1, $2, $3, $4)`, userID, conn, strconv.FormatInt(id, 10), login))
}

func TestAccess(t *testing.T) {
	f := newFixture(t)
	testkit.AccessStoreSuite(t, f.st, accessSeed{f}, f.account, f.project.ID)
}

func TestJobsInProjects(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t)
	for _, tt := range []struct {
		in   []uuid.UUID
		want int
	}{{nil, 1}, {[]uuid.UUID{}, 0}, {[]uuid.UUID{f.project.ID}, 1}, {[]uuid.UUID{domain.NewID()}, 0}} {
		got, err := f.st.Jobs().List(ctx, f.account, store.JobFilter{InProjects: tt.in})
		must(t, err)
		if len(got) != tt.want || (tt.want == 1 && got[0].ID != j.ID) {
			t.Errorf("InProjects %v = %d jobs", tt.in, len(got))
		}
	}
}
