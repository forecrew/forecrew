//go:build integration

package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestTeamMigration applies 00004 on a schema with a goal and a job in it:
// the goal becomes a feature goal, and the migration rolls back cleanly.
func TestTeamMigration(t *testing.T) {
	pool := newPoolAt(t, 3)
	ctx := t.Context()
	var goal, job string
	must(t, pool.QueryRow(ctx, `WITH a AS (SELECT id FROM accounts LIMIT 1),
		pr AS (INSERT INTO projects (account_id, name, repo, status) SELECT id, 'p', 'o/p', 'registered' FROM a RETURNING id, account_id)
		INSERT INTO goals (account_id, project_id, number, title, status, created_by)
		SELECT account_id, id, 1, 'Ship', 'draft', 'user:me' FROM pr RETURNING id`).Scan(&goal))
	must(t, pool.QueryRow(ctx, `INSERT INTO jobs (account_id, project_id, number, status, role, runtime, spec)
		SELECT account_id, project_id, 1, 'queued', 'developer', 'claude-code', '{}' FROM goals RETURNING id`).Scan(&job))

	p := provider(t, pool)
	_, err := p.UpTo(ctx, 4)
	must(t, err)
	var kind string
	var size, manifest *string
	must(t, pool.QueryRow(ctx, `SELECT kind, size::text FROM goals WHERE id = $1`, goal).Scan(&kind, &size))
	must(t, pool.QueryRow(ctx, `SELECT context_manifest::text FROM jobs WHERE id = $1`, job).Scan(&manifest))
	if kind != string(domain.GoalFeature) || size != nil || manifest != nil {
		t.Fatalf("after migration: kind %q, size %v, manifest %v", kind, size, manifest)
	}
	mustFail(t, pool, `UPDATE goals SET kind = 'epic'`)
	must(t, exec(pool, `UPDATE goals SET kind = 'bug', size = '{"impact":"low"}'`))

	_, err = p.DownTo(ctx, 3)
	must(t, err)
	_, err = p.Up(ctx)
	must(t, err)
}

// TestTeamRefCounters: the new ref kinds are allocated like the old ones.
func TestTeamRefCounters(t *testing.T) {
	f := newFixture(t)
	for _, k := range domain.RefKinds() {
		for want := int64(1); want <= 2; want++ {
			var got int64
			must(t, f.pool.QueryRow(t.Context(), `INSERT INTO ref_counters (account_id, kind, last) VALUES ($1, $2, 1)
				ON CONFLICT (account_id, kind) DO UPDATE SET last = ref_counters.last + 1 RETURNING last`, f.account, string(k)).Scan(&got))
			if got != want {
				t.Fatalf("%s: got %d, want %d", k, got, want)
			}
		}
	}
	mustFail(t, f.pool, `INSERT INTO ref_counters (account_id, kind) SELECT id, 'ADR' FROM accounts`)
}

// TestTeamKindsMatchDomain: every gate and signal kind of the domain is
// accepted by the CHECKs.
func TestTeamKindsMatchDomain(t *testing.T) {
	f := newFixture(t)
	for i, k := range domain.GateKinds() {
		must(t, exec(f.pool, `INSERT INTO gates (account_id, project_id, number, kind, status, title, subject_repo)
			VALUES ($1, $2, $3, $4, 'pending', 't', 'o/r')`, f.account, f.project.ID, i+1, string(k)))
	}
	for _, k := range domain.SignalKinds() {
		must(t, exec(f.pool, `INSERT INTO signals (account_id, project_id, kind, key, dedupe_key, payload)
			VALUES ($1, $2, $3, 'k', $3, '{}')`, f.account, f.project.ID, string(k)))
	}
}

// TestTeamTables checks the constraints of the new tables.
func TestTeamTables(t *testing.T) {
	f := newFixture(t)
	g := f.newGoal(t)
	a, p, goal, job := f.account, f.project.ID, g.ID, f.newJob(t).ID
	hash := strings.Repeat("a", 64)
	tests := []struct {
		name string
		sql  string
		args []any
		ok   bool
	}{
		{"council", `INSERT INTO councils (account_id, project_id, goal_id, number, kind, question, status) VALUES ($1, $2, $3, 1, 'technical', 'Which queue?', 'running')`, []any{a, p, goal}, true},
		{"decided council without brief", `INSERT INTO councils (account_id, project_id, number, kind, question, status) VALUES ($1, $2, 2, 'adhoc', 'q', 'decided')`, []any{a, p}, false},
		{"council kind", `INSERT INTO councils (account_id, project_id, number, kind, question, status) VALUES ($1, $2, 3, 'debate', 'q', 'running')`, []any{a, p}, false},
		{"revision round 2", `INSERT INTO council_contributions (account_id, council_id, role, stage, round, content) SELECT $1, id, 'architect', 'revision', 2, '{}' FROM councils`, []any{a}, true},
		{"revision round 4", `INSERT INTO council_contributions (account_id, council_id, role, stage, round, content) SELECT $1, id, 'architect', 'revision', 4, '{}' FROM councils`, []any{a}, false},
		{"critique round 2", `INSERT INTO council_contributions (account_id, council_id, role, stage, round, content) SELECT $1, id, 'architect', 'critique', 2, '{}' FROM councils`, []any{a}, false},
		{"human revision round 3", `INSERT INTO council_contributions (account_id, council_id, role, stage, round, content, evidence_check) SELECT $1, id, 'architect', 'revision', 3, '{}', '[{"source":"docs/adr/1.md","kind":"path","status":"missing"}]' FROM councils`, []any{a}, true},
		{"same member, stage and round", `INSERT INTO council_contributions (account_id, council_id, role, stage, round, content) SELECT $1, id, 'architect', 'revision', 3, '{}' FROM councils`, []any{a}, false},
		{"same role on another runtime", `INSERT INTO council_contributions (account_id, council_id, role, runtime, stage, round, content) SELECT $1, id, 'architect', 'codex', 'revision', 3, '{}' FROM councils`, []any{a}, true},
		{"same role and runtime again", `INSERT INTO council_contributions (account_id, council_id, role, runtime, stage, round, content) SELECT $1, id, 'architect', 'codex', 'revision', 3, '{}' FROM councils`, []any{a}, false},
		{"second synthesis", `INSERT INTO council_contributions (account_id, council_id, role, stage, round, content) SELECT $1, id, 'facilitator', 'synthesis', 2, '{}' FROM councils`, []any{a}, true},
		{"third synthesis", `INSERT INTO council_contributions (account_id, council_id, role, stage, round, content) SELECT $1, id, 'facilitator', 'synthesis', 3, '{}' FROM councils`, []any{a}, false},
		{"ask_human gate", `INSERT INTO gates (account_id, project_id, number, kind, origin, status, title, subject_repo, job_id) VALUES ($1, $2, 900, 'needs_you', 'ask_human', 'pending', 'JOB asks', 'o/r', $3)`, []any{a, p, job}, true},
		{"ask_human gate without job", `INSERT INTO gates (account_id, project_id, number, kind, origin, status, title, subject_repo) VALUES ($1, $2, 901, 'needs_you', 'ask_human', 'pending', 't', 'o/r')`, []any{a, p}, false},
		{"ask_human spec gate", `INSERT INTO gates (account_id, project_id, number, kind, origin, status, title, subject_repo, job_id) VALUES ($1, $2, 902, 'spec_approval', 'ask_human', 'pending', 't', 'o/r', $3)`, []any{a, p, job}, false},
		{"assumption", `INSERT INTO assumptions (account_id, project_id, number, goal_id, statement, made_by, confidence, impact, status, verify_by) VALUES ($1, $2, 1, $3, 'One instance', 'architect', 'medium', 'high', 'unverified', 'test')`, []any{a, p, goal}, true},
		{"resolved without time", `INSERT INTO assumptions (account_id, project_id, number, statement, made_by, confidence, impact, status, verify_by) VALUES ($1, $2, 2, 's', 'architect', 'low', 'low', 'accepted', 'research')`, []any{a, p}, false},
		{"verify_by", `INSERT INTO assumptions (account_id, project_id, number, statement, made_by, confidence, impact, status, verify_by) VALUES ($1, $2, 3, 's', 'architect', 'low', 'low', 'unverified', 'guess')`, []any{a, p}, false},
		{"decision", `INSERT INTO decisions (account_id, project_id, number, question, choice, decided_by) VALUES ($1, $2, 1, 'q', 'postgres', 'user:ana')`, []any{a, p}, true},
		{"policy decision without council", `INSERT INTO decisions (account_id, project_id, number, question, choice, decided_by) VALUES ($1, $2, 2, 'q', 'postgres', 'policy')`, []any{a, p}, false},
		{"entry", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body, body_hash) VALUES ($1, $2, 'repo', 'docs/adr/0001.md', 'architecture', 'Queue', 'We use Postgres queues with SKIP LOCKED', $3)`, []any{a, p, hash}, true},
		{"duplicate path", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body_hash) VALUES ($1, $2, 'repo', 'docs/adr/0001.md', 'architecture', 'x', $3)`, []any{a, p, hash}, false},
		{"goal note", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body_hash, scope, goal_id, tags) VALUES ($1, $2, 'internal', 'n1', 'notes', 'Flaky test', $3, 'goal', $4, '{ci}')`, []any{a, p, hash, goal}, true},
		{"goal note without goal", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body_hash, scope) VALUES ($1, $2, 'internal', 'n2', 'notes', 'x', $3, 'goal')`, []any{a, p, hash}, false},
		{"note without scope", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body_hash) VALUES ($1, $2, 'internal', 'n3', 'notes', 'x', $3)`, []any{a, p, hash}, false},
		{"repo entry in notes", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body_hash) VALUES ($1, $2, 'repo', 'n4', 'notes', 'x', $3)`, []any{a, p, hash}, false},
		{"repo entry with goal", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body_hash, goal_id) VALUES ($1, $2, 'repo', 'n5', 'lessons', 'x', $3, $4)`, []any{a, p, hash, goal}, false},
		{"generated outside its section", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body_hash, generated) VALUES ($1, $2, 'repo', 'n6', 'lessons', 'x', $3, true)`, []any{a, p, hash}, false},
		{"bad hash", `INSERT INTO knowledge_entries (account_id, project_id, provider, path, section, title, body_hash) VALUES ($1, $2, 'repo', 'n7', 'lessons', 'x', 'nope')`, []any{a, p}, false},
		{"knowledge proposal", `INSERT INTO knowledge_proposals (account_id, project_id, number, provider, section, path, action, body, reason, proposed_by, goal_id, batch, status) VALUES ($1, $2, 1, 'repo', 'conventions', 'CONTRIBUTING.md', 'edit', 'x', 'r', 'reviewer', $3, 'per_goal', 'queued')`, []any{a, p, goal}, true},
		{"proposal to generated", `INSERT INTO knowledge_proposals (account_id, project_id, number, provider, section, path, action, reason, proposed_by, batch, status) VALUES ($1, $2, 2, 'repo', 'generated', 'x', 'add', 'r', 'reviewer', 'standalone', 'queued')`, []any{a, p}, false},
		{"proposal to notes", `INSERT INTO knowledge_proposals (account_id, project_id, number, provider, section, path, action, reason, proposed_by, batch, status) VALUES ($1, $2, 3, 'internal', 'notes', 'x', 'add', 'r', 'reviewer', 'standalone', 'queued')`, []any{a, p}, false},
		{"per_goal without goal", `INSERT INTO knowledge_proposals (account_id, project_id, number, provider, section, path, action, reason, proposed_by, batch, status) VALUES ($1, $2, 4, 'repo', 'lessons', 'x', 'add', 'r', 'reviewer', 'per_goal', 'queued')`, []any{a, p}, false},
		{"proposal", insertProposal(1, "proposed", "NULL"), []any{a, p}, true},
		{"same dedupe key while open", insertProposal(2, "snoozed", "now()"), []any{a, p}, false},
		{"snoozed without until", insertProposal(3, "snoozed", "NULL"), []any{a, p}, false},
	}
	for _, tt := range tests {
		err := exec(f.pool, tt.sql, tt.args...)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
	// Rejecting frees the dedupe key.
	must(t, exec(f.pool, `UPDATE proposals SET status = 'rejected'`))
	must(t, exec(f.pool, insertProposal(4, "proposed", "NULL"), a, p))

	var hits int
	must(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM knowledge_entries
		WHERE search @@ websearch_to_tsquery('english', 'postgres queue')`).Scan(&hits))
	if hits != 1 {
		t.Fatalf("full-text hits = %d, want 1", hits)
	}
}

// TestTeamEmbedding: knowledge_entries.embedding exists exactly when the
// migration could install pgvector (available, and the role may create
// it); otherwise the migration still succeeds without it.
func TestTeamEmbedding(t *testing.T) {
	f := newFixture(t)
	var available, installed, column bool
	must(t, f.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector'),
		EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector'),
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'knowledge_entries' AND column_name = 'embedding')`).
		Scan(&available, &installed, &column))
	if column != installed || (installed && !available) {
		t.Fatalf("pgvector available %v, installed %v, embedding column %v", available, installed, column)
	}
	t.Logf("pgvector available %v, installed %v", available, installed)
}

func insertProposal(number int, status, snoozedUntil string) string {
	return fmt.Sprintf(`INSERT INTO proposals (account_id, project_id, number, scope, category, title, rationale, benefit, effort, risk, action, status, dedupe_key, snoozed_until)
		VALUES ($1, $2, %d, 'project', 'workers', 't', 'r', 'b', 's', 'low', '{"setup":{"instructions":"x"}}', '%s', 'workers:macos', %s)`, number, status, snoozedUntil)
}

// newPoolAt returns a fresh database migrated up to version.
func newPoolAt(t *testing.T, version int64) *pgxpool.Pool {
	t.Helper()
	pool := testkit.NewDatabase(t)
	_, err := provider(t, pool).UpTo(t.Context(), version)
	must(t, err)
	return pool
}

func provider(t *testing.T, pool *pgxpool.Pool) *goose.Provider {
	t.Helper()
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS())
	must(t, err)
	return p
}

func exec(pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(context.Background(), sql, args...)
	return err
}

func mustFail(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if err := exec(pool, sql, args...); err == nil {
		t.Fatalf("accepted: %s", sql)
	}
}

// TestJobMCPTokenHash: the job MCP token hash lives only as long as the
// lease it was issued with. Whatever ends the lease (complete, cancel,
// release, expiry) or replaces it (a new lease) clears it, through the
// store's own queries, unchanged by T31.
func TestJobMCPTokenHash(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	w := f.newWorker(t, "w", "runtime:claude-code")
	jobs := f.st.Jobs()
	hash := strings.Repeat("ab", 32)
	lease := func(t *testing.T) domain.Job {
		t.Helper()
		j, err := jobs.LeaseNext(ctx, f.account, w.ID, []string{"runtime:claude-code"}, time.Minute)
		must(t, err)
		must(t, exec(f.pool, `UPDATE jobs SET job_mcp_token_hash = decode($2, 'hex') WHERE id = $1`, j.ID, hash))
		if stored(t, f, j.ID) == nil {
			t.Fatal("hash not stored on a running job")
		}
		return j
	}
	ends := map[string]func(t *testing.T, j domain.Job){
		"complete": func(t *testing.T, j domain.Job) {
			_, err := jobs.Complete(ctx, f.account, j.ID, w.ID, store.JobResult{Status: domain.JobSucceeded})
			must(t, err)
		},
		"cancel": func(t *testing.T, j domain.Job) {
			_, err := jobs.Cancel(ctx, f.account, j.ID)
			must(t, err)
		},
		"release": func(t *testing.T, j domain.Job) {
			_, err := jobs.Release(ctx, f.account, j.ID, w.ID, "retry")
			must(t, err)
		},
		"lease expiry": func(t *testing.T, j domain.Job) {
			must(t, exec(f.pool, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, j.ID))
			_, err := jobs.RequeueExpired(ctx, f.account)
			must(t, err)
		},
		"new lease": func(t *testing.T, j domain.Job) {
			must(t, exec(f.pool, `UPDATE jobs SET worker_id = $2, started_at = now() + interval '1 second' WHERE id = $1`, j.ID, w.ID))
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			f.newJob(t)
			j := lease(t)
			end(t, j)
			if got := stored(t, f, j.ID); got != nil {
				t.Fatalf("hash kept after %s: %x", name, got)
			}
			must(t, exec(f.pool, `UPDATE jobs SET status = 'canceled', worker_id = NULL, lease_expires_at = NULL WHERE id = $1 AND status <> 'canceled' AND status <> 'succeeded'`, j.ID))
		})
	}
	j := f.newJob(t)
	must(t, exec(f.pool, `UPDATE jobs SET job_mcp_token_hash = decode($2, 'hex') WHERE id = $1`, j.ID, hash))
	if got := stored(t, f, j.ID); got != nil {
		t.Fatalf("hash kept on a queued job: %x", got)
	}
	mustFail(t, f.pool, `INSERT INTO jobs (account_id, project_id, number, status, role, runtime, spec, job_mcp_token_hash)
		VALUES ($1, $2, 999, 'queued', 'r', 'claude-code', '{}', '\x01')`, f.account, f.project.ID)
}

func stored(t *testing.T, f *fixture, job uuid.UUID) []byte {
	t.Helper()
	var h []byte
	must(t, f.pool.QueryRow(t.Context(), `SELECT job_mcp_token_hash FROM jobs WHERE id = $1`, job).Scan(&h))
	return h
}

func TestCouncils(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Councils()
	goal := f.newGoal(t)
	c := domain.Council{AccountID: f.account, ProjectID: f.project.ID, GoalID: goal.ID, Kind: domain.CouncilTechnical, Question: "Which queue?"}
	must(t, repo.Create(ctx, &c))
	if c.Number == 0 || c.Status != domain.CouncilRunning || c.CreatedAt.IsZero() {
		t.Fatalf("Create = %+v", c)
	}
	dup := domain.Council{ID: c.ID, AccountID: f.account, ProjectID: f.project.ID, Kind: domain.CouncilAdhoc, Question: "q"}
	wantErr(t, repo.Create(ctx, &dup), store.ErrConflict)

	run := domain.WorkflowRun{ID: c.ID, AccountID: f.account, ProjectID: f.project.ID, Playbook: "council"}
	must(t, f.st.WorkflowRuns().Create(ctx, &run))
	gate := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateCouncilDecision, Title: "t", SubjectRepo: "acme/forecrew"}
	must(t, f.st.Gates().Create(ctx, &gate))
	must(t, f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		cur, err := tx.Councils().GetForUpdate(ctx, f.account, c.ID)
		if err != nil {
			return err
		}
		cur.Constraints = []string{"keep Postgres"}
		cur.Brief = &domain.CouncilBrief{Question: "Which queue?", Recommendation: "pg", Reversible: true}
		cur.GateID, cur.RunID = gate.ID, run.ID
		return tx.Councils().Update(ctx, &cur)
	}))
	got, err := repo.GetByNumber(ctx, f.account, c.Number)
	if err != nil || len(got.Constraints) != 1 || got.Brief == nil || got.Brief.Recommendation != "pg" || got.GateID != gate.ID || got.RunID != run.ID || got.GoalID != goal.ID {
		t.Fatalf("GetByNumber = %+v, %v", got, err)
	}
	must(t, repo.UpdateStatus(ctx, f.account, c.ID, domain.CouncilRunning, domain.CouncilDecided))
	wantErr(t, repo.UpdateStatus(ctx, f.account, c.ID, domain.CouncilRunning, domain.CouncilFailed), store.ErrConflict)

	bare := domain.Council{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.CouncilAdhoc, Question: "q"}
	must(t, repo.Create(ctx, &bare))
	if b, err := repo.Get(ctx, f.account, bare.ID); err != nil || b.Brief != nil || b.GoalID != uuid.Nil() || b.Constraints == nil {
		t.Fatalf("Get = %+v, %v", b, err)
	}
	list, err := repo.List(ctx, f.account, store.CouncilFilter{ProjectID: f.project.ID, Status: domain.CouncilDecided, GoalID: goal.ID, GateID: gate.ID})
	if err != nil || len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if list, _ := repo.List(ctx, f.account, store.CouncilFilter{Limit: 1}); len(list) != 1 || list[0].ID != bare.ID {
		t.Fatalf("newest = %+v", list)
	}
	wantErr(t, f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		_, err := tx.Councils().GetForUpdate(ctx, f.account, domain.NewID())
		return err
	}), store.ErrNotFound)
}

func TestCouncilContributions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Councils()
	c := domain.Council{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.CouncilAdhoc, Question: "q"}
	must(t, repo.Create(ctx, &c))
	job := f.newJob(t)
	p := domain.CouncilContribution{AccountID: f.account, CouncilID: c.ID, Role: "architect", Runtime: "codex", Stage: domain.StageProposal, Round: 1, JobID: job.ID, Content: []byte(`{"position":"pg"}`)}
	must(t, repo.AddContribution(ctx, &p))
	same := p
	same.ID, same.Runtime = uuid.Nil(), "claude-code"
	wantErr(t, repo.AddContribution(ctx, &same), store.ErrConflict)
	same.JobID, same.Runtime = uuid.Nil(), "codex"
	wantErr(t, repo.AddContribution(ctx, &same), store.ErrConflict)
	same.Stage = domain.StageCritique
	same.EvidenceCheck = []domain.SourceCheck{}
	must(t, repo.AddContribution(ctx, &same))

	check := []domain.SourceCheck{{Source: "a.go", Kind: domain.SourcePath, Status: domain.SourceMissing, Detail: "gone"}}
	must(t, repo.SetEvidenceCheck(ctx, f.account, p.ID, check))
	all, err := repo.Contributions(ctx, f.account, c.ID, "")
	if err != nil || len(all) != 2 || !strings.Contains(string(all[0].Content), "pg") || len(all[0].EvidenceCheck) != 1 ||
		all[0].EvidenceCheck[0].Status != domain.SourceMissing || all[1].EvidenceCheck == nil {
		t.Fatalf("Contributions = %+v, %v", all, err)
	}
	if got, _ := repo.Contributions(ctx, f.account, c.ID, domain.StageCritique); len(got) != 1 || got[0].JobID != uuid.Nil() {
		t.Fatalf("critiques = %+v", got)
	}
	must(t, repo.SetEvidenceCheck(ctx, f.account, same.ID, nil))
	wantErr(t, repo.SetEvidenceCheck(ctx, f.account, domain.NewID(), nil), store.ErrNotFound)
}
