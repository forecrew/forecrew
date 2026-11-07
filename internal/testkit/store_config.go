package testkit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func configPlansT(d *storeData) *table[domain.ConfigPlan]              { return &d.configPlans }
func conversationsT(d *storeData) *table[domain.AssistantConversation] { return &d.conversations }

// ConfigPlans implements store.Store.
func (s *Store) ConfigPlans() store.ConfigPlanRepo { return configPlanRepo{s} }

type configPlanRepo struct{ s *Store }

func (r configPlanRepo) Create(_ context.Context, p *domain.ConfigPlan) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		p.ID = newID(p.ID)
		p.CreatedAt, p.UpdatedAt = now, now
		d.configPlans.insert(*p)
		return nil
	})
}

func (r configPlanRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.ConfigPlan, error) {
	return getRow(r.s, configPlansT, accountID, id)
}

func (r configPlanRepo) List(_ context.Context, accountID uuid.UUID, f store.ConfigPlanFilter) ([]domain.ConfigPlan, error) {
	return read(r.s, func(d *storeData) ([]domain.ConfigPlan, error) {
		return d.configPlans.newest(f.Limit, func(p domain.ConfigPlan) bool {
			return p.AccountID == accountID && zeroOr(f.Status, p.Status) && zeroOr(f.CreatedBy, p.CreatedBy) && zeroOr(f.ConversationID, p.ConversationID) && (!f.Open || p.Status.Open())
		}), nil
	})
}

func (r configPlanRepo) ListExpired(_ context.Context, now time.Time, limit int) ([]domain.ConfigPlan, error) {
	return listRows(r.s, configPlansT, limit, func(p domain.ConfigPlan) bool { return p.Status.Open() && !now.Before(p.ExpiresAt) })
}

func (r configPlanRepo) Move(_ context.Context, p *domain.ConfigPlan, from domain.ConfigPlanStatus) error {
	if _, err := from.Transition(p.Status); err != nil {
		return err
	}
	return update(r.s, configPlansT, p.AccountID, p.ID, func(row *domain.ConfigPlan, now time.Time) error {
		if row.Status != from {
			return fmt.Errorf("config plan %s is not %s: %w", p.ID, from, store.ErrConflict)
		}
		row.Status, row.Changes, row.AppliedBy, row.AppliedAt, row.PRURL, row.UpdatedAt = p.Status, p.Changes, p.AppliedBy, p.AppliedAt, p.PRURL, now
		return nil
	})
}

// ConfigSettings implements store.Store.
func (s *Store) ConfigSettings() store.ConfigSettingRepo { return configSettingRepo{s} }

type configSettingRepo struct{ s *Store }

func sortedSettings(rows []domain.ConfigSetting) []domain.ConfigSetting {
	slices.SortFunc(rows, func(a, b domain.ConfigSetting) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return rows
}

func (r configSettingRepo) List(_ context.Context, accountID uuid.UUID, kind string) ([]domain.ConfigSetting, error) {
	return read(r.s, func(d *storeData) ([]domain.ConfigSetting, error) {
		return sortedSettings(d.settings.filter(0, func(s domain.ConfigSetting) bool { return s.AccountID == accountID && s.Kind == kind })), nil
	})
}

func (r configSettingRepo) ListAll(_ context.Context, accountID uuid.UUID) ([]domain.ConfigSetting, error) {
	return read(r.s, func(d *storeData) ([]domain.ConfigSetting, error) {
		return sortedSettings(d.settings.filter(0, func(s domain.ConfigSetting) bool { return s.AccountID == accountID })), nil
	})
}

func (r configSettingRepo) Put(_ context.Context, s *domain.ConfigSetting) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		row, err := d.settings.find(func(x domain.ConfigSetting) bool {
			return x.AccountID == s.AccountID && x.Kind == s.Kind && x.Name == s.Name
		})
		if err != nil {
			s.ID = newID(s.ID)
			s.CreatedAt, s.UpdatedAt = now, now
			d.settings.insert(*s)
			return nil
		}
		s.ID, s.CreatedAt, s.UpdatedAt = row.ID, row.CreatedAt, now
		*row = *s
		return nil
	})
}

func (r configSettingRepo) Delete(_ context.Context, accountID uuid.UUID, kind, name string) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		i := slices.IndexFunc(d.settings.rows, func(x domain.ConfigSetting) bool {
			return x.AccountID == accountID && x.Kind == kind && x.Name == name
		})
		if i < 0 {
			return fmt.Errorf("setting %s/%s: %w", kind, name, store.ErrNotFound)
		}
		d.settings.rows = slices.Delete(d.settings.rows, i, i+1)
		return nil
	})
}

// Secrets implements store.Store.
func (s *Store) Secrets() store.SecretRepo { return secretRepo{s} }

type secretRepo struct{ s *Store }

func (r secretRepo) Put(_ context.Context, s *domain.StoredSecret) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		row, err := d.secrets.find(func(x domain.StoredSecret) bool { return x.AccountID == s.AccountID && x.Name == s.Name })
		if err != nil {
			s.ID = newID(s.ID)
			s.CreatedAt, s.UpdatedAt = now, now
			d.secrets.insert(*s)
			return nil
		}
		s.ID, s.CreatedAt, s.UpdatedAt = row.ID, row.CreatedAt, now
		*row = *s
		return nil
	})
}

func (r secretRepo) Get(_ context.Context, accountID uuid.UUID, name string) (domain.StoredSecret, error) {
	return read(r.s, func(d *storeData) (domain.StoredSecret, error) {
		row, err := d.secrets.find(func(x domain.StoredSecret) bool { return x.AccountID == accountID && x.Name == name })
		if err != nil {
			return domain.StoredSecret{}, fmt.Errorf("secret %s: %w", name, err)
		}
		return *row, nil
	})
}

func (r secretRepo) ListStale(_ context.Context, keyID string, limit int) ([]domain.StoredSecret, error) {
	return read(r.s, func(d *storeData) ([]domain.StoredSecret, error) {
		return d.secrets.filter(limit, func(s domain.StoredSecret) bool { return s.KeyID != keyID }), nil
	})
}

func (r secretRepo) Delete(_ context.Context, accountID uuid.UUID, name string) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		i := slices.IndexFunc(d.secrets.rows, func(x domain.StoredSecret) bool { return x.AccountID == accountID && x.Name == name })
		if i < 0 {
			return fmt.Errorf("secret %s: %w", name, store.ErrNotFound)
		}
		d.secrets.rows = slices.Delete(d.secrets.rows, i, i+1)
		return nil
	})
}

// Conversations implements store.Store.
func (s *Store) Conversations() store.ConversationRepo { return conversationRepo{s} }

type conversationRepo struct{ s *Store }

func (r conversationRepo) Create(_ context.Context, c *domain.AssistantConversation) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := foreign(&d.projects, c.AccountID, c.ProjectID); err != nil {
			return err
		}
		c.ID = newID(c.ID)
		c.CreatedAt, c.UpdatedAt = now, now
		d.conversations.insert(*c)
		return nil
	})
}

func (r conversationRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.AssistantConversation, error) {
	return getRow(r.s, conversationsT, accountID, id)
}

func (r conversationRepo) GetByJob(_ context.Context, accountID, jobID uuid.UUID) (domain.AssistantConversation, error) {
	return read(r.s, func(d *storeData) (domain.AssistantConversation, error) {
		m, err := d.messages.find(func(m domain.AssistantMessage) bool { return m.AccountID == accountID && m.JobID == jobID })
		if err != nil {
			return domain.AssistantConversation{}, err
		}
		c, err := d.conversations.get(accountID, m.ConversationID)
		if err != nil {
			return domain.AssistantConversation{}, err
		}
		return *c, nil
	})
}

func (r conversationRepo) List(_ context.Context, accountID uuid.UUID, owner string, limit int) ([]domain.AssistantConversation, error) {
	return read(r.s, func(d *storeData) ([]domain.AssistantConversation, error) {
		return d.conversations.newest(limit, func(c domain.AssistantConversation) bool { return c.AccountID == accountID && c.Owner == owner }), nil
	})
}

func (r conversationRepo) AddMessage(_ context.Context, m *domain.AssistantMessage) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		c, err := d.conversations.get(m.AccountID, m.ConversationID)
		if err != nil {
			return err
		}
		if m.JobID != uuid.Nil() && d.messages.exists(func(x domain.AssistantMessage) bool { return x.JobID == m.JobID && x.Author == m.Author }) {
			return fmt.Errorf("job %s has its %s message: %w", m.JobID, m.Author, store.ErrConflict)
		}
		m.ID = newID(m.ID)
		m.CreatedAt = now
		c.UpdatedAt = now
		d.messages.insert(*m)
		return nil
	})
}

func (r conversationRepo) Messages(_ context.Context, accountID, conversationID uuid.UUID) ([]domain.AssistantMessage, error) {
	return read(r.s, func(d *storeData) ([]domain.AssistantMessage, error) {
		return d.messages.filter(0, func(m domain.AssistantMessage) bool {
			return m.AccountID == accountID && m.ConversationID == conversationID
		}), nil
	})
}

// ModelConnections implements store.Store.
func (s *Store) ModelConnections() store.ModelConnectionRepo { return modelConnectionRepo{s} }

type modelConnectionRepo struct{ s *Store }

func (r modelConnectionRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.ModelConnection, error) {
	return read(r.s, func(d *storeData) ([]domain.ModelConnection, error) {
		rows := d.modelConns.filter(0, func(c domain.ModelConnection) bool { return c.AccountID == accountID })
		slices.SortFunc(rows, func(a, b domain.ModelConnection) int { return strings.Compare(a.Name, b.Name) })
		return rows, nil
	})
}

func (r modelConnectionRepo) Put(_ context.Context, c *domain.ModelConnection) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		row, err := d.modelConns.find(func(x domain.ModelConnection) bool { return x.AccountID == c.AccountID && x.Name == c.Name })
		if err != nil {
			c.ID = newID(c.ID)
			c.CreatedAt, c.UpdatedAt = now, now
			d.modelConns.insert(*c)
			return nil
		}
		c.ID, c.CreatedAt, c.UpdatedAt = row.ID, row.CreatedAt, now
		*row = *c
		return nil
	})
}

func (r modelConnectionRepo) Delete(_ context.Context, accountID uuid.UUID, name string) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		i := slices.IndexFunc(d.modelConns.rows, func(x domain.ModelConnection) bool { return x.AccountID == accountID && x.Name == name })
		if i < 0 {
			return fmt.Errorf("model connection %s: %w", name, store.ErrNotFound)
		}
		d.modelConns.rows = slices.Delete(d.modelConns.rows, i, i+1)
		return nil
	})
}

// ModelAssignments implements store.Store.
func (s *Store) ModelAssignments() store.ModelAssignmentRepo { return modelAssignmentRepo{s} }

type modelAssignmentRepo struct{ s *Store }

func (r modelAssignmentRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.ModelAssignment, error) {
	return read(r.s, func(d *storeData) ([]domain.ModelAssignment, error) {
		rows := d.modelAssigns.filter(0, func(a domain.ModelAssignment) bool { return a.AccountID == accountID })
		slices.SortFunc(rows, func(a, b domain.ModelAssignment) int {
			return strings.Compare(a.ProjectID.String(), b.ProjectID.String())
		})
		return rows, nil
	})
}

func (r modelAssignmentRepo) Put(_ context.Context, a *domain.ModelAssignment) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := foreign(&d.projects, a.AccountID, a.ProjectID); err != nil {
			return err
		}
		row, err := d.modelAssigns.find(func(x domain.ModelAssignment) bool { return x.AccountID == a.AccountID && x.ProjectID == a.ProjectID })
		if err != nil {
			a.ID = newID(a.ID)
			a.CreatedAt, a.UpdatedAt = now, now
			d.modelAssigns.insert(*a)
			return nil
		}
		a.ID, a.CreatedAt, a.UpdatedAt = row.ID, row.CreatedAt, now
		*row = *a
		return nil
	})
}

func (r modelAssignmentRepo) Delete(_ context.Context, accountID, projectID uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		i := slices.IndexFunc(d.modelAssigns.rows, func(x domain.ModelAssignment) bool { return x.AccountID == accountID && x.ProjectID == projectID })
		if i < 0 {
			return fmt.Errorf("model assignment: %w", store.ErrNotFound)
		}
		d.modelAssigns.rows = slices.Delete(d.modelAssigns.rows, i, i+1)
		return nil
	})
}
