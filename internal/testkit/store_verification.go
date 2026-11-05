package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func verificationsT(d *storeData) *table[domain.VerificationReport] { return &d.verifications }

// VerificationReports implements store.Store.
func (s *Store) VerificationReports() store.VerificationReportRepo { return verificationRepo{s} }

type verificationRepo struct{ s *Store }

func (r verificationRepo) Upsert(_ context.Context, v *domain.VerificationReport) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, v.AccountID, v.ProjectID), foreign(&d.changes, v.AccountID, v.ChangeID), foreign(&d.artifacts, v.AccountID, v.ArtifactID)); err != nil {
			return err
		}
		v.Report = bytes.Clone(v.Report)
		cur, err := d.verifications.find(func(x domain.VerificationReport) bool {
			return x.AccountID == v.AccountID && x.ChangeID == v.ChangeID && x.SHA == v.SHA
		})
		if err != nil {
			v.ID, v.CreatedAt, v.UpdatedAt = newID(v.ID), now, now
			d.verifications.insert(*v)
			return nil
		}
		cur.Status, cur.Report, cur.ArtifactID, cur.UpdatedAt = v.Status, json.RawMessage(bytes.Clone(v.Report)), v.ArtifactID, now
		v.ID, v.CreatedAt, v.UpdatedAt = cur.ID, cur.CreatedAt, now
		return nil
	})
}

func (r verificationRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.VerificationReport, error) {
	return getRow(r.s, verificationsT, accountID, id)
}

func (r verificationRepo) GetBySHA(_ context.Context, accountID, changeID uuid.UUID, sha string) (domain.VerificationReport, error) {
	return findRow(r.s, verificationsT, func(x domain.VerificationReport) bool {
		return x.AccountID == accountID && x.ChangeID == changeID && x.SHA == sha
	})
}

func (r verificationRepo) Latest(_ context.Context, accountID, changeID uuid.UUID) (domain.VerificationReport, error) {
	return read(r.s, func(d *storeData) (domain.VerificationReport, error) {
		all := d.verifications.filter(0, func(x domain.VerificationReport) bool { return x.AccountID == accountID && x.ChangeID == changeID })
		if len(all) == 0 {
			return domain.VerificationReport{}, store.ErrNotFound
		}
		return slices.MaxFunc(all, func(a, b domain.VerificationReport) int { return a.UpdatedAt.Compare(b.UpdatedAt) }), nil
	})
}
