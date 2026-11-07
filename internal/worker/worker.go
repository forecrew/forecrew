package worker

import (
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/runtime"
)

// DefaultGracePeriod is how long running jobs may finish after shutdown
// starts.
const DefaultGracePeriod = 10 * time.Minute

// DefaultImageMaxAge is how long an environment image is kept after its last
// use.
const DefaultImageMaxAge = config.DefaultImagePruneDays * 24 * time.Hour

// Runtimes looks runtime adapters up by name; *runtime.Registry implements
// it.
type Runtimes interface {
	Get(name string) (runtime.Adapter, error)
}

// Config holds the dependencies and settings of a Worker.
type Config struct {
	Client   *Client
	Labels   []string // every label of the worker, see DetectLabels
	Version  string
	WorkDir  string
	MaxJobs  int
	Runtimes Runtimes
	Analyzer analyzer.Analyzer
	Logger   *slog.Logger
	// Environ is the environment of the worker's own git processes
	// (os.Environ() in production).
	Environ []string

	// The fields below are optional.

	// CloneURL returns the URL of a repo; the default is
	// https://github.com/<repo>.git.
	CloneURL func(repo string) string
	// GracePeriod is how long running jobs may finish after shutdown starts;
	// zero means DefaultGracePeriod.
	GracePeriod time.Duration
	// HeartbeatInterval overrides the interval the server asks for.
	HeartbeatInterval time.Duration
	// FlushInterval is how often events are sent; zero means 2 s.
	FlushInterval time.Duration
	Now           func() time.Time
	// DockerBin is the docker client to use; empty means this worker has no
	// Docker (it does not advertise the docker label), see newFromConfig.
	DockerBin string
	// ImageMaxAge is how long an environment image is kept after its last
	// use; zero means config.DefaultImagePruneDays days.
	ImageMaxAge time.Duration
	// RegistryAuth lets Docker use the operator's registry logins; without
	// it Docker runs with an empty config in work_dir/docker-config.
	RegistryAuth bool
	// Toolboxes holds the files of packs/toolboxes (Dockerfiles and what they
	// copy), the build contexts of the toolbox images; nil means this
	// worker can only pull toolbox images.
	Toolboxes fs.FS
	// DockerHost is the daemon endpoint of the operator's Docker context,
	// kept when the config is replaced; see newFromConfig.
	DockerHost string
	// LocalModelEndpoints lets a key check reach a model on localhost or a
	// private address (local_model_endpoints in worker.yaml).
	LocalModelEndpoints bool
	// agents installs the agent CLIs forecrewd asks for; nil means
	// auto_install_agents is off, or the worker is on a fleet machine.
	agents *agentInstaller
	// fleet marks the worker of a fleet machine: its agents come from the
	// image.
	fleet bool
}

// Worker polls forecrewd and runs its jobs.
type Worker struct {
	cfg    Config
	git    *gitClient
	reaper *reaper
	docker *dockerCLI // nil without Docker

	mu     sync.Mutex
	labels []string // Config.Labels, and the runtimes installed since
}

// New returns a Worker. Config.Client, Runtimes, Analyzer and Logger are
// required.
func New(cfg Config) *Worker {
	if cfg.CloneURL == nil {
		cfg.CloneURL = func(repo string) string { return "https://github.com/" + repo + ".git" }
	}
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = DefaultGracePeriod
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.MaxJobs < 1 {
		cfg.MaxJobs = 1
	}
	if cfg.ImageMaxAge <= 0 {
		cfg.ImageMaxAge = DefaultImageMaxAge
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if abs, err := filepath.Abs(cfg.WorkDir); err == nil {
		cfg.WorkDir = abs
	}
	// Git never searches for a repo above the work dir's parent.
	g := newGitClient("git", nil, cfg.Environ, filepath.Dir(cfg.WorkDir))
	w := &Worker{cfg: cfg, git: g, reaper: newReaper(g.runningPIDs, &g.gate), labels: cfg.Labels}
	if cfg.DockerBin != "" {
		w.docker = newDockerCLI(cfg.DockerBin, cfg.WorkDir, cfg.Environ)
		if !cfg.RegistryAuth {
			w.docker.isolate(filepath.Join(cfg.WorkDir, "docker-config"), cfg.DockerHost)
		}
	}
	return w
}
