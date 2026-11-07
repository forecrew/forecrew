// Package postgres implements store.Store on Postgres with pgx and
// hand-written SQL (docs/contracts/data-model.md).
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/forecrew/forecrew/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies the embedded migrations. It is safe to run concurrently
// and again on an up-to-date database, where it does nothing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	err := migrateDB(ctx, db)
	if cerr := db.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("migrate: close: %w", cerr)
	}
	return err
}

func migrateDB(ctx context.Context, db *sql.DB) error {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS(), goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("migrate: provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: up: %w", err)
	}
	return nil
}

// dbtx is what both *pgxpool.Pool and pgx.Tx offer.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store is the Postgres store.Store.
type Store struct {
	db  dbtx
	vec *vectorColumn
}

var _ store.Store = (*Store)(nil)

// New returns a store on pool. The schema must have been migrated.
func New(pool *pgxpool.Pool) *Store { return &Store{db: pool, vec: &vectorColumn{}} }

// WithTx implements store.Store. Nested calls use savepoints.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx store.Store) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		return fn(ctx, &Store{db: tx, vec: s.vec})
	})
}

// Accounts implements store.Store.
func (s *Store) Accounts() store.AccountRepo { return accountRepo{s.db} }

// ConfigPlans implements store.Store.
func (s *Store) ConfigPlans() store.ConfigPlanRepo { return configPlanRepo{s.db} }

// ConfigSettings implements store.Store.
func (s *Store) ConfigSettings() store.ConfigSettingRepo { return configSettingRepo{s.db} }

// Secrets implements store.Store.
func (s *Store) Secrets() store.SecretRepo { return secretRepo{s.db} }

// Conversations implements store.Store.
func (s *Store) Conversations() store.ConversationRepo { return conversationRepo{s.db} }

// Projects implements store.Store.
func (s *Store) Projects() store.ProjectRepo { return projectRepo{s.db} }

// Goals implements store.Store.
func (s *Store) Goals() store.GoalRepo { return goalRepo{s.db} }

// Specs implements store.Store.
func (s *Store) Specs() store.SpecRepo { return specRepo{s.db} }

// Tasks implements store.Store.
func (s *Store) Tasks() store.TaskRepo { return taskRepo{s.db} }

// Changes implements store.Store.
func (s *Store) Changes() store.ChangeRepo { return changeRepo{s.db} }

// Jobs implements store.Store.
func (s *Store) Jobs() store.JobRepo { return jobRepo{s.db} }

// JobEvents implements store.Store.
func (s *Store) JobEvents() store.JobEventRepo { return jobEventRepo{s.db} }

// Gates implements store.Store.
func (s *Store) Gates() store.GateRepo { return gateRepo{s.db} }

// Signals implements store.Store.
func (s *Store) Signals() store.SignalRepo { return signalRepo{s.db} }

// Workers implements store.Store.
func (s *Store) Workers() store.WorkerRepo { return workerRepo{s.db} }

// WorkflowRuns implements store.Store.
func (s *Store) WorkflowRuns() store.WorkflowRunRepo { return workflowRunRepo{s.db} }

// StepRuns implements store.Store.
func (s *Store) StepRuns() store.StepRunRepo { return stepRunRepo{s.db} }

// WebhookDeliveries implements store.Store.
func (s *Store) WebhookDeliveries() store.WebhookDeliveryRepo { return webhookRepo{s.db} }

// MCPTokens implements store.Store.
func (s *Store) MCPTokens() store.PersonalTokenRepo { return mcpTokenRepo{s.db} }

// Audit implements store.Store.
func (s *Store) Audit() store.AuditRepo { return auditRepo{s.db} }

// Artifacts implements store.Store.
func (s *Store) Artifacts() store.ArtifactRepo { return artifactRepo{s.db} }

// TestRuns implements store.Store.
func (s *Store) TestRuns() store.TestRunRepo { return testRunRepo{s.db} }

// VerificationReports implements store.Store.
func (s *Store) VerificationReports() store.VerificationReportRepo {
	return verificationReportRepo{s.db}
}

// Councils implements store.Store.
func (s *Store) Councils() store.CouncilRepo { return councilRepo{s.db} }

// Proposals implements store.Store.
func (s *Store) Proposals() store.ProposalRepo { return proposalRepo{s.db} }

// Postgres error codes the store maps to sentinels.
const (
	codeUnique     = "23505"
	codeForeignKey = "23503"
	codeCheck      = "23514"
)

// lastOwnerConstraint is the constraint name the owner triggers of
// migration 00005 raise.
const lastOwnerConstraint = "last_owner"

// mapErr converts driver errors into the store sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pg.Code {
		case codeUnique:
			return fmt.Errorf("%w: %s", store.ErrConflict, pg.ConstraintName)
		case codeForeignKey:
			return fmt.Errorf("%w: %s", store.ErrNotFound, pg.ConstraintName)
		case codeCheck:
			if pg.ConstraintName == lastOwnerConstraint {
				return store.ErrLastOwner
			}
		}
	}
	return err
}

// nextNumber allocates the next human ref number of kind within the
// caller's transaction.
func nextNumber(ctx context.Context, db dbtx, accountID uuid.UUID, kind string) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, `INSERT INTO ref_counters (account_id, kind, last) VALUES ($1, $2, 1)
		ON CONFLICT (account_id, kind) DO UPDATE SET last = ref_counters.last + 1
		RETURNING last`, accountID, kind).Scan(&n)
	return n, err
}

func migrationsFS() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

// Teams implements store.Store.
func (s *Store) Teams() store.TeamRepo { return teamRepo{s.db} }

// ServiceAccounts implements store.Store.
func (s *Store) ServiceAccounts() store.ServiceAccountRepo { return serviceAccountRepo{s.db} }

// Roles implements store.Store.
func (s *Store) Roles() store.RoleRepo { return roleRepo{s.db} }

// RoleBindings implements store.Store.
func (s *Store) RoleBindings() store.RoleBindingRepo { return roleBindingRepo{s.db} }

// Directory implements store.Store.
func (s *Store) Directory() store.DirectoryRepo { return directoryRepo{s.db} }

// Knowledge implements store.Store.
func (s *Store) Knowledge() store.KnowledgeRepo { return knowledgeRepo{s.db, s.vec} }
