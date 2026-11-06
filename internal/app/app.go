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
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/actionsprovider"
	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetapi"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/scheduler"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/tools"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/githubissues"
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
	// Config is the configuration in effect: forecrew.yaml with the stored
	// settings over it.
	Config *config.Config
	// File is forecrew.yaml as read, what stored settings go over; it
	// defaults to Config.
	File *config.Config
	// Getenv reads the environment; it defaults to os.Getenv.
	Getenv func(string) string
	Store  store.Store
	// Ping reports whether the database is reachable, for /healthz.
	Ping   func(context.Context) error
	GitHub github.Tracker
	// Actions is GitHub's Actions and file surface; with it forecrewd runs
	// the jobs of pool actions and installs the workflow. Nil: pool actions
	// has no provider and its jobs wait.
	Actions github.Actions
	Logger  *slog.Logger
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
	// GitHubURL and GitHubAPIURL replace github.com for sign-in; tests only.
	GitHubURL, GitHubAPIURL string
	// Embedder adds semantic similarity to knowledge search; it defaults
	// to OpenAI when EnvEmbeddingsKey is set, and nil without either.
	Embedder knowledge.Embedder
	// FleetEvery is how often the fleet reconciles
	// (fleet.ReconcileInterval).
	FleetEvery time.Duration
	// FleetDirect serves direct-mode capacity providers; nil serves none.
	FleetDirect func(domain.CapacityProvider) fleet.Provider
}

// EnvEmbeddingsKey is the OpenAI API key of the knowledge embeddings
// (docs/knowledge.md). It is optional: search works on words without it.
const EnvEmbeddingsKey = "FORECREW_EMBEDDINGS_API_KEY" // #nosec G101 -- variable name, not a secret

// App is a forecrewd, ready to run.
type App struct {
	d       Deps
	handler http.Handler
	jobs    *jobs.Service
	fleet   *fleet.Controller
	catalog *roles.Catalog
	creator *jobCreator
	tools   *tools.Service
	// vault is the secret store; nil without a key.
	vault  *secrets.Encrypted
	auth   *auth.Service
	engine *workflow.Engine
	// retention deletes expired artifacts and orphan objects; nil without
	// evidence storage.
	retention *evidence.Retention
	// provider runs the jobs of pool actions; nil without Deps.Actions.
	provider *actionsprovider.Provider
	// runEngine runs the engine; tests replace it.
	runEngine func(context.Context) error
	webhooks  *webhook.Handler
	poller    *webhook.Poller
	// notes sweeps expired agent notes daily.
	notes *knowledge.Notes
	// schedules starts the playbooks of forecrew.yaml's schedules.
	schedules *scheduler.Scheduler
	// syncs reindexes projects' knowledge off the webhook goroutine.
	syncs *knowledge.Scheduler
	// tickets takes work from trackers and sends status back.
	tickets *tracker.Service
	certs   *certManager
	closers []func()
	// httpAddr is where the ACME challenge and redirect listen when TLS is on.
	httpAddr string
}

var _ jobs.FailureRegistrar = (*proposals.Service)(nil)

// Open connects to Postgres, migrates the schema and builds the App from
// cfg. Call Close when done.
func Open(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string, opts Options) (*App, error) {
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
	st := postgres.New(pool)
	effective, err := withStoredSettings(ctx, cfg, st, os.Getenv, logger, opts)
	if err != nil {
		pool.Close()
		return nil, err
	}
	a, err := New(Deps{Config: effective, File: cfg, Store: st, Ping: pool.Ping, GitHub: gh, Actions: gh, Logger: logger, Version: version})
	if err != nil {
		pool.Close()
		return nil, err
	}
	a.closers = append(a.closers, pool.Close)
	return a, nil
}

// warnDeprecated logs the settings of cfg that are on their way out.
func warnDeprecated(cfg *config.Config, log *slog.Logger) {
	if len(cfg.GitHub.Allowlist) > 0 {
		log.Warn("github.allowlist is deprecated: link GitHub identities and add people to the organization instead")
	}
}

// checkSchedule refuses a schedule of a playbook forecrewd cannot start.
func checkSchedule(books map[string]*workflow.Playbook, sc config.Schedule) error {
	book, ok := books[sc.Playbook]
	switch {
	case !ok:
		return fmt.Errorf("%s: there is no such playbook", sc)
	case !slices.Equal(book.Inputs, []string{"repo"}):
		return fmt.Errorf("%s: a scheduled playbook takes the input repo and no other, and %s takes %v", sc, sc.Playbook, book.Inputs)
	}
	return nil
}

// newSchedules builds the scheduler of forecrew.yaml's schedules. Startup
// refuses a schedule of a playbook that does not exist rather than letting it
// fail every minute.
func newSchedules(list []config.Schedule, books map[string]*workflow.Playbook, st store.Store, engine *workflow.Engine, log *slog.Logger, now func() time.Time) (*scheduler.Scheduler, error) {
	for _, sc := range list {
		if err := checkSchedule(books, sc); err != nil {
			return nil, fmt.Errorf("forecrew.yaml schedules: %w", err)
		}
	}
	s, err := scheduler.New(list, st, engine)
	if err != nil {
		return nil, err
	}
	s.Log, s.Now = log, now
	return s, nil
}

// Close releases what Open acquired.
func (a *App) Close() {
	for _, c := range slices.Backward(a.closers) {
		c()
	}
	a.closers = nil
}

// withDefaults fills in what Deps leaves out.
func (d Deps) withDefaults() Deps {
	if d.Files == nil {
		d.Files = forecrew.Files
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.ReapEvery <= 0 {
		d.ReapEvery = jobs.ReapInterval
	}
	if d.FleetEvery <= 0 {
		d.FleetEvery = fleet.ReconcileInterval
	}
	if d.RetainEvery <= 0 {
		d.RetainEvery = evidence.SweepInterval
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.File == nil {
		d.File = d.Config
	}
	return d
}

// New builds every component. It fails when a role or a playbook is
// invalid: a playbook calling an unregistered action or running an unknown
// role stops startup.
func New(d Deps) (*App, error) {
	d = d.withDefaults()
	cfg, st, log := d.Config, d.Store, d.Logger
	ev, err := openEvidence(d)
	if err != nil {
		return nil, err
	}
	catalog, err := roles.New(d.Files, cfg)
	if err != nil {
		return nil, fmt.Errorf("load roles: %w", err)
	}
	seats, err := councils.Load(d.Files, catalog.Definition)
	if err != nil {
		return nil, fmt.Errorf("load councils: %w", err)
	}
	books, err := workflow.LoadPlaybooks(d.Files, playbooksDir)
	if err != nil {
		return nil, err
	}
	if err := checkRoles(books, catalog); err != nil {
		return nil, err
	}

	bus := workflow.NewBus(st.Signals())
	regs := registers.New(st, bus, d.Now)
	machines := fleet.New(fleet.Config{Store: st, Logger: log, Now: d.Now, Direct: d.FleetDirect, ServerURL: cfg.Server.PublicBaseURL, Currency: cfg.Fleet.Currency,
		Image: images.Resolver(st.Images())})
	jobSvc := jobs.New(st, bus, cfg.Pools, log, d.Now).WithRegistrar(regs).WithWaiting(machines)
	gateSvc := gates.New(st, bus, d.Now).WithRecorder(regs)
	emb := d.Embedder
	if emb == nil && os.Getenv(EnvEmbeddingsKey) != "" {
		emb = &knowledge.OpenAI{Key: os.Getenv(EnvEmbeddingsKey)}
	}
	index := &knowledge.Index{Store: st, Embedder: emb, Log: log}
	syncer := &knowledge.Syncer{
		Store: st, Index: index, API: d.GitHub, Log: log, Now: d.Now,
		Snapshots: knowledge.ArchiveSnapshots{Archiver: d.GitHub, Analyzer: analyzer.Local{}},
	}
	notes := &knowledge.Notes{Store: st, Index: index, Now: d.Now, Log: log}
	search := &knowledge.Search{Index: index, Policy: syncer.Policy}
	kbProposals := &proposals.Service{Store: st, Policy: syncer.Policy, GitHub: d.GitHub, Gates: gateSvc, Roles: catalog, Log: log}
	gateSvc.WithLessons(kbProposals)
	jobSvc.WithRegistrar(kbProposals) // also hears of failed and canceled jobs (jobs.FailureRegistrar)
	tasks, err := roles.LoadTasks(d.Files)
	if err != nil {
		return nil, err
	}
	creator := &jobCreator{tasks: tasks, store: st, roles: catalog, packs: contextpack.New(st, d.GitHub, cfg.GitHub.Allowed, 0).WithKnowledge(search).WithAssumptions(regs), jobs: jobSvc, knowledge: syncer, councils: seats, models: cfg.ModelOn}

	actions := workflow.Actions{}
	var engine *workflow.Engine
	playbooks.Register(actions, playbooks.Deps{
		Store: st, Tracker: d.GitHub, Signals: bus, Sizing: cfg.Sizing, Roles: catalog, Knowledge: syncer, Proposals: kbProposals, Now: d.Now, Actions: d.Actions, Packs: d.Files,
		Config: cfg, Councils: seats, Gates: gateSvc, Evidence: ev, Logger: log,
		Runs: lateStarter(func() *workflow.Engine { return engine }),
	})
	engine, err = workflow.New(workflow.Config{
		Store: st, Bus: bus, Jobs: creator, Gates: gateSvc, Actions: actions, Playbooks: books, Logger: log, Now: d.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("start workflow engine: %w", err)
	}
	access := authz.New(st, log).WithProtection(kbProposals)
	trackers := tracker.NewRegistry()
	trackers.Register(githubissues.Kind, githubissues.Factory(githubissues.Deps{GitHub: d.GitHub, Store: st, Secret: []byte(cfg.GitHub.WebhookSecret), Now: d.Now}))
	tickets := tracker.New(tracker.Config{
		Store: st, Registry: trackers, Gates: gateSvc, Authz: access, Runs: engine, MaxActiveGoals: cfg.Trackers.MaxActiveGoals,
		Playbook: func(p domain.Project, k domain.GoalKind) string {
			return tools.GoalPlaybook(cfg.Testing.Projects, cfg.GoalV3.Projects, p, k)
		},
		PublicURL: cfg.Server.PublicBaseURL, Logger: log, Now: d.Now,
	})
	encrypted, err := openSecrets(cfg, st)
	if err != nil {
		return nil, err
	}
	var vault secrets.Store // a nil *Encrypted must stay a nil interface
	if encrypted != nil {
		vault = encrypted
	}
	authSvc := auth.New(auth.Config{Store: st, Auth: cfg.Auth, BaseURL: cfg.Server.PublicBaseURL, Logger: log, Now: d.Now, GitHubURL: d.GitHubURL, GitHubAPIURL: d.GitHubAPIURL, Secrets: vault})
	toolSvc := tools.New(tools.Config{
		Store: st, Gates: gateSvc, Jobs: jobSvc, Creator: creator, Playbooks: engine, Roles: catalog, GitHub: d.GitHub, Evidence: ev, Search: search, Notes: notes, Proposals: kbProposals,
		Registers: regs, Councils: seats, TestingProjects: cfg.Testing.Projects, GoalV3Projects: cfg.GoalV3.Projects, Fleet: machines, Logger: log, PublicURL: cfg.Server.PublicBaseURL, TokenTTL: cfg.Auth.TokenDefaultTTL, Now: d.Now,
		GitHubUserID: authSvc.GitHubUserID, Server: d.File, Getenv: d.Getenv, Repo: d.GitHub, Secrets: vault, Trackers: tickets,
		CheckSchedule: func(sc config.Schedule) error { return checkSchedule(books, sc) },
	})
	syncs := &knowledge.Scheduler{Syncer: syncer, Log: log, Now: d.Now}
	hooks := webhook.Deps{Store: st, Signals: bus, Gates: gateSvc, Authz: access, Tracker: d.GitHub, Knowledge: syncs, Proposals: kbProposals, Bugs: toolSvc, Tickets: tickets, Allowed: cfg.GitHub.Allowed, Log: log, Now: d.Now}
	jobGateway := jobmcp.New(jobmcp.Config{
		Store: st, Gates: gateSvc, Bus: bus, Evidence: ev != nil, Assumptions: regs, Search: search, Notes: notes, Proposals: kbProposals, Assistant: toolSvc, Logger: log, Version: d.Version, Now: d.Now,
	})
	workers := workerapi.New(workerapi.Config{
		Store: st, Jobs: jobSvc, Tokens: d.GitHub, Env: workerapi.RuntimeEnv(cfg), Endpoint: workerapi.RuntimeEndpoint(cfg), Pools: cfg.Pools, Logger: log,
		Evidence: ev, MaxArtifactSize: evidenceMaxSize(cfg), MaxTestResultsSize: cfg.MaxTestResultsSize,
		JobMCPURL: jobMCPURL(cfg), Attachments: jobGateway.Attachments(),
	})

	var provider *actionsprovider.Provider
	if d.Actions != nil {
		provider = actionsprovider.New(actionsprovider.Config{
			Store: st, Jobs: jobSvc, GitHub: d.Actions, Evidence: ev, MaxInFlight: cfg.Actions.MaxInFlight,
			MaxArtifactSize: evidenceMaxSize(cfg), Logger: log, Now: d.Now,
		})
		hooks.RunChanged = provider.Notify
	}
	schedules, err := newSchedules(cfg.Schedules, books, st, engine, log, d.Now)
	if err != nil && d.File != cfg {
		// A stored schedule never stops the server, as no stored setting does.
		log.Error("stored schedules are ignored, running on forecrew.yaml", "error", err)
		schedules, err = newSchedules(d.File.Schedules, books, st, engine, log, d.Now)
	}
	if err != nil {
		return nil, err
	}
	warnDeprecated(cfg, log)
	a := &App{
		auth:      authSvc,
		schedules: schedules, syncs: syncs, tickets: tickets, provider: provider, d: d, jobs: jobSvc, fleet: machines, catalog: catalog, creator: creator, tools: toolSvc, engine: engine, runEngine: engine.Run, httpAddr: ":80", notes: notes,
		webhooks: webhook.NewHandler([]byte(cfg.GitHub.WebhookSecret), hooks),
		poller:   webhook.NewPoller(hooks),
	}
	if ev != nil && cfg.Evidence != nil {
		a.retention = &evidence.Retention{Store: st, Evidence: ev, Days: cfg.Evidence.RetentionDays, Logger: log, Now: d.Now}
	}
	if cfg.Server.TLSDomain != "" {
		a.certs = newCertManager(cfg.Server.TLSDomain, cfg.Server.CertCacheDir)
	}
	agents := fleetapi.New(fleetapi.Config{Store: st, Fleet: machines, Logger: log, Now: d.Now})
	a.handler = a.routes(workers.Handler(), agents.Handler(), jobGateway.Handler(),
		mcp.NewHandler(toolSvc, st, access, d.Version, mcp.NewResource(cfg.Server.PublicBaseURL), log),
		api.New(api.Config{
			Tools: toolSvc, Artifacts: toolSvc, Images: toolSvc, Store: st, Authz: access, Auth: a.auth, Logger: log, PublicURL: cfg.Server.PublicBaseURL,
			TokenLogin: cfg.Auth.TokenLogin, SessionMaxAge: cfg.Auth.SessionAbsolute, Now: d.Now,
		}).Handler())
	return a, nil
}

// lateStarter starts runs on the engine, which is built after the actions
// that use it.
type lateStarter func() *workflow.Engine

// Start implements councils.Starter.
func (s lateStarter) Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	return s().Start(ctx, playbook, req)
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
func (a *App) routes(workers, agents, jobMCP, mcpHandler, apiHandler http.Handler) http.Handler {
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
	mux.Handle("/fleet/v1/", agents)
	mux.Handle("/auth/", a.auth.Handler())
	mux.Handle("/api/v1/", apiHandler)
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	// The job MCP gateway: agents of running jobs, with job tokens.
	mux.Handle(jobmcp.Path, jobMCP)
	mux.Handle("/webhooks/github", a.webhooks)
	mux.Handle(tracker.WebhookPath, a.tickets)
	mux.Handle("/", webui.Handler())
	return mux
}

// Run serves HTTP and runs the loops until ctx ends, then stops them. It
// returns an error when a server or the workflow engine fails; everything
// stops with it.
func (a *App) Run(ctx context.Context) error {
	if err := a.auth.Start(ctx); err != nil {
		return fmt.Errorf("start sign-in: %w", err)
	}
	a.rotateSecrets(ctx)
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
	wg.Go(func() { a.fleet.Run(runCtx, a.d.FleetEvery, a.jobs.Wait) })
	wg.Go(func() { a.webhooks.Run(runCtx) })
	wg.Go(func() { a.poller.Run(runCtx) })
	wg.Go(func() { a.tickets.Run(runCtx) })
	if a.provider != nil {
		wg.Go(func() { a.provider.Run(runCtx) })
	}
	wg.Go(func() { a.notes.RunSweep(runCtx, knowledge.SweepEvery) })
	wg.Go(func() { a.syncs.Run(runCtx) })
	wg.Go(func() { a.auth.RunSweep(runCtx, auth.SweepEvery) })
	wg.Go(func() { a.schedules.Run(runCtx) })
	wg.Go(func() { a.tools.RunConfigSweep(runCtx) })
	if a.retention != nil {
		wg.Go(func() { a.retention.Run(runCtx, a.d.RetainEvery) })
	}
	err := a.serve(runCtx)
	stop()
	wg.Wait()
	return errors.Join(err, engineErr)
}

// Options are what the command line says about a start.
type Options struct {
	// IgnoreStoredSettings starts forecrewd although stored settings do not
	// pass: those are left out, and every one is logged as an error.
	IgnoreStoredSettings bool
}

// withStoredSettings returns cfg with the settings stored over the file. It
// fails closed: a stored value can be stricter than the file, a tighter pool
// or shorter sessions, so running on the file instead would quietly loosen
// the server. opts.IgnoreStoredSettings is the way past a bad row: the others
// stay, so what is stricter stays too.
func withStoredSettings(ctx context.Context, cfg *config.Config, st store.Store, getenv func(string) string, log *slog.Logger, opts Options) (*config.Config, error) {
	settings, err := resource.Stored(ctx, st)
	if err != nil {
		if !opts.IgnoreStoredSettings {
			return nil, fmt.Errorf("read stored settings: %w; fix them or start with --ignore-stored-settings", err)
		}
		log.ErrorContext(ctx, "STORED SETTINGS IGNORED: they could not be read, running on forecrew.yaml alone", "error", err)
		return cfg, nil
	}
	if len(settings) == 0 {
		return cfg, nil
	}
	merged, err := cfg.WithSettings(settings, getenv)
	if err == nil {
		return merged, nil
	}
	if !opts.IgnoreStoredSettings {
		return nil, fmt.Errorf("stored settings do not pass: %w; fix them or start with --ignore-stored-settings", err)
	}
	kept := make([]config.Setting, 0, len(settings))
	for _, s := range settings {
		if _, err := cfg.WithSettings(append(slices.Clone(kept), s), getenv); err != nil {
			log.ErrorContext(ctx, "STORED SETTING IGNORED: this server runs without it", "kind", s.Kind, "name", s.Name, "error", err)
			continue
		}
		kept = append(kept, s)
	}
	return cfg.WithSettings(kept, getenv)
}

// openSecrets opens the secret store; without a key it is off and secure
// fields answer unavailable.
func openSecrets(cfg *config.Config, st store.Store) (*secrets.Encrypted, error) {
	if cfg.SecretKey == "" {
		return nil, nil
	}
	key, err := secrets.ParseKey(cfg.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvSecretKey, err)
	}
	var previous [][]byte
	if cfg.SecretKeyPrevious != "" {
		old, err := secrets.ParseKey(cfg.SecretKeyPrevious)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", config.EnvSecretKeyPrevious, err)
		}
		previous = append(previous, old)
	}
	return secrets.New(st.Secrets(), key, previous...)
}

// rotateSecrets moves values to the current key of the secret store. A
// failure is logged: values under a key the server holds still read.
func (a *App) rotateSecrets(ctx context.Context) {
	if a.vault == nil {
		return
	}
	n, err := a.vault.Rotate(ctx)
	switch {
	case err != nil:
		a.d.Logger.ErrorContext(ctx, "rotate the secret store", "moved", n, "error", err)
	case n > 0:
		a.d.Logger.InfoContext(ctx, "secret store rotated", "moved", n)
	}
}
