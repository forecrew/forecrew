// Package app assembles forecrewd: it builds every component from the
// config and runs the HTTP server and the background loops (workflow
// engine, lease reaper, webhook queue and reconciliation poller). forecrewd
// is a single replica in the MVP.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/tools"
	"github.com/forecrew/forecrew/internal/webhook"
	"github.com/forecrew/forecrew/internal/webui"
	"github.com/forecrew/forecrew/internal/workerapi"
	"github.com/forecrew/forecrew/internal/workflow"
)

// ShutdownTimeout bounds how long in-flight requests may take on shutdown.
const ShutdownTimeout = 10 * time.Second

// playbooksDir holds the playbooks in the embedded packs.
const playbooksDir = "packs/playbooks"

// Deps are what App is built from.
type Deps struct {
	Config *config.Config
	Store  store.Store
	// Ping reports whether the database is reachable, for /healthz.
	Ping   func(context.Context) error
	GitHub github.Tracker
	Logger *slog.Logger
	// Version is reported to MCP clients.
	Version string
	// Files holds packs/ (role and playbook files); it defaults to the
	// packs embedded in the binary.
	Files fs.FS
	// Listener, when set, is used instead of listening on Config.Server.Listen.
	Listener net.Listener
	Now      func() time.Time // defaults to the real UTC time
	// ReapEvery is how often expired leases are looked for (jobs.ReapInterval).
	ReapEvery time.Duration
	// Evidence stores artifacts; it defaults to the S3 storage of
	// Config.Evidence, and nil without either.
	Evidence evidence.Store
	// RetainEvery is how often expired artifacts are swept
	// (evidence.SweepInterval).
	RetainEvery time.Duration
}

// App is a forecrewd, ready to run.
type App struct {
	d       Deps
	handler http.Handler
	jobs    *jobs.Service
	catalog *roles.Catalog
	creator *jobCreator
	tools   *tools.Service
	engine  *workflow.Engine
	// retention deletes expired artifacts and orphan objects; nil without
	// evidence storage.
	retention *evidence.Retention
	// runEngine runs the engine; tests replace it.
	runEngine func(context.Context) error
	webhooks  *webhook.Handler
	poller    *webhook.Poller
	certs     *certManager
	closers   []func()
	// httpAddr is where the ACME challenge and redirect listen when TLS is on.
	httpAddr string
}

// Open connects to Postgres, migrates the schema and builds the App from
// cfg. Call Close when done.
func Open(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string) (*App, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	gh, err := github.New(github.Config{AppID: cfg.GitHub.AppID, PrivateKey: []byte(cfg.GitHub.PrivateKey)})
	if err != nil {
		pool.Close()
		return nil, err
	}
	a, err := New(Deps{Config: cfg, Store: postgres.New(pool), Ping: pool.Ping, GitHub: gh, Logger: logger, Version: version})
	if err != nil {
		pool.Close()
		return nil, err
	}
	a.closers = append(a.closers, pool.Close)
	return a, nil
}

// Close releases what Open acquired.
func (a *App) Close() {
	for _, c := range slices.Backward(a.closers) {
		c()
	}
	a.closers = nil
}

// New builds every component. It fails when a role or a playbook is
// invalid: a playbook calling an unregistered action or running an unknown
// role stops startup.
func New(d Deps) (*App, error) {
	if d.Files == nil {
		d.Files = forecrew.Files
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.ReapEvery <= 0 {
		d.ReapEvery = jobs.ReapInterval
	}
	if d.RetainEvery <= 0 {
		d.RetainEvery = evidence.SweepInterval
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	cfg, st, log := d.Config, d.Store, d.Logger
	ev, err := openEvidence(d)
	if err != nil {
		return nil, err
	}
	catalog, err := roles.New(d.Files, cfg)
	if err != nil {
		return nil, fmt.Errorf("load roles: %w", err)
	}
	books, err := workflow.LoadPlaybooks(d.Files, playbooksDir)
	if err != nil {
		return nil, err
	}
	if err := checkRoles(books, catalog); err != nil {
		return nil, err
	}

	bus := workflow.NewBus(st.Signals())
	jobSvc := jobs.New(st, bus, cfg.Pools, log, d.Now)
	gateSvc := gates.New(st, bus, d.Now)
	creator := &jobCreator{store: st, roles: catalog, packs: contextpack.New(st, d.GitHub, cfg.GitHub.Allowed, 0), jobs: jobSvc}

	actions := workflow.Actions{}
	playbooks.Register(actions, playbooks.Deps{Store: st, Tracker: d.GitHub, Signals: bus, Sizing: cfg.Sizing, Roles: catalog, Now: d.Now})
	engine, err := workflow.New(workflow.Config{
		Store: st, Bus: bus, Jobs: creator, Gates: gateSvc, Actions: actions, Playbooks: books, Logger: log, Now: d.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("start workflow engine: %w", err)
	}
	toolSvc := tools.New(tools.Config{
		Store: st, Gates: gateSvc, Jobs: jobSvc, Creator: creator, Playbooks: engine, Roles: catalog, GitHub: d.GitHub, Evidence: ev, Logger: log,
	})
	hooks := webhook.Deps{Store: st, Signals: bus, Gates: gateSvc, Tracker: d.GitHub, Allowed: cfg.GitHub.Allowed, Log: log, Now: d.Now}
	jobGateway := jobmcp.New(jobmcp.Config{
		Store: st, Gates: gateSvc, Bus: bus, Evidence: ev != nil, Logger: log, Version: d.Version, Now: d.Now,
	})
	workers := workerapi.New(workerapi.Config{
		Store: st, Jobs: jobSvc, Tokens: d.GitHub, Env: workerapi.RuntimeEnv(cfg), Pools: cfg.Pools, Logger: log,
		Evidence: ev, MaxArtifactSize: evidenceMaxSize(cfg), MaxTestResultsSize: cfg.MaxTestResultsSize,
		JobMCPURL: jobMCPURL(cfg), Attachments: jobGateway.Attachments(),
	})

	a := &App{
		d: d, jobs: jobSvc, catalog: catalog, creator: creator, tools: toolSvc, engine: engine, runEngine: engine.Run, httpAddr: ":80",
		webhooks: webhook.NewHandler([]byte(cfg.GitHub.WebhookSecret), hooks),
		poller:   webhook.NewPoller(hooks),
	}
	if ev != nil && cfg.Evidence != nil {
		a.retention = &evidence.Retention{Store: st, Evidence: ev, Days: cfg.Evidence.RetentionDays, Logger: log, Now: d.Now}
	}
	if cfg.Server.TLSDomain != "" {
		a.certs = newCertManager(cfg.Server.TLSDomain, cfg.Server.CertCacheDir)
	}
	a.handler = a.routes(workers.Handler(), jobGateway.Handler(),
		mcp.NewHandler(toolSvc, st.MCPTokens(), d.Version, log),
		api.New(api.Config{Tools: toolSvc, Artifacts: toolSvc, Tokens: st.MCPTokens(), Accounts: st.Accounts(), Logger: log}).Handler())
	return a, nil
}

// openEvidence returns the evidence storage of d: the given one, the S3
// storage of the config, or nil when artifacts are not configured.
func openEvidence(d Deps) (evidence.Store, error) {
	if d.Evidence != nil || d.Config.Evidence == nil {
		return d.Evidence, nil
	}
	s3, err := evidence.NewS3(d.Config.Evidence)
	if err != nil {
		return nil, err
	}
	d.Logger.Info("evidence storage configured", "endpoint", d.Config.Evidence.Endpoint, "bucket", d.Config.Evidence.Bucket,
		"retention_days", d.Config.Evidence.RetentionDays)
	return s3, nil
}

// jobMCPURL is the job MCP gateway on the public URL, "" without one. Job
// specs carry it for information; workers use their own server URL.
func jobMCPURL(cfg *config.Config) string {
	if cfg.Server.PublicBaseURL == "" {
		return ""
	}
	return cfg.Server.PublicBaseURL + jobmcp.Path
}

// evidenceMaxSize is the artifact size limit of the config, 0 (the default)
// without evidence storage.
func evidenceMaxSize(cfg *config.Config) int64 {
	if cfg.Evidence == nil {
		return 0
	}
	return cfg.Evidence.MaxArtifactSize
}

// checkRoles fails when a job step of a playbook runs a role that does not
// exist. Roles given by a reference are checked when the job is created.
func checkRoles(books map[string]*workflow.Playbook, c *roles.Catalog) error {
	var check func(playbook string, steps []workflow.Step) error
	check = func(playbook string, steps []workflow.Step) error {
		for _, s := range steps {
			if role, ok := s.With["role"].(string); ok && s.Type == workflow.StepJob && !strings.Contains(role, "${") {
				if _, err := c.Definition(role); err != nil {
					return fmt.Errorf("playbook %q step %q: %w", playbook, s.ID, err)
				}
			}
			if err := check(playbook, s.Steps); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(books)) {
		p := books[name]
		if err := check(name, append(slices.Clone(p.Steps), p.Finally...)); err != nil {
			return err
		}
	}
	return nil
}

// Handler returns the HTTP handler of forecrewd.
func (a *App) Handler() http.Handler { return a.handler }

// routes mounts everything on one mux; "/" is the web UI.
func (a *App) routes(workers, jobMCP, mcpHandler, apiHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if a.d.Ping != nil {
			if err := a.d.Ping(r.Context()); err != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/worker/v1/", workers)
	mux.Handle("/api/v1/", apiHandler)
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	// The job MCP gateway: agents of running jobs, with job tokens.
	mux.Handle(jobmcp.Path, jobMCP)
	mux.Handle("/webhooks/github", a.webhooks)
	mux.Handle("/", webui.Handler())
	return mux
}

// Run serves HTTP and runs the loops until ctx ends, then stops them. It
// returns an error when a server or the workflow engine fails; everything
// stops with it.
func (a *App) Run(ctx context.Context) error {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	var engineErr error
	wg.Go(func() {
		if engineErr = a.runEngine(runCtx); engineErr != nil {
			a.d.Logger.Error("workflow engine stopped", "error", engineErr)
			stop()
		}
	})
	wg.Go(func() { a.jobs.RunReaper(runCtx, a.d.ReapEvery) })
	wg.Go(func() { a.webhooks.Run(runCtx) })
	wg.Go(func() { a.poller.Run(runCtx) })
	if a.retention != nil {
		wg.Go(func() { a.retention.Run(runCtx, a.d.RetainEvery) })
	}
	err := a.serve(runCtx)
	stop()
	wg.Wait()
	return errors.Join(err, engineErr)
}
