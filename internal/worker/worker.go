package worker

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/runtime"
)

// DefaultGracePeriod is how long running jobs may finish after shutdown
// starts.
const DefaultGracePeriod = 10 * time.Minute

// DefaultImageMaxAge is how long an environment image is kept after its last
// use.
const DefaultImageMaxAge = 14 * 24 * time.Hour

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
	// Docker (it does not advertise the docker label), see DetectDocker.
	DockerBin string
	// ImageMaxAge is how long an environment image is kept after its last
	// use; zero means 14 days.
	ImageMaxAge time.Duration
}

// Worker polls forecrewd and runs its jobs.
type Worker struct {
	cfg    Config
	git    *gitClient
	reaper *reaper
	docker *dockerCLI // nil without Docker
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
	w := &Worker{cfg: cfg, git: g, reaper: newReaper(g.runningPIDs, &g.gate)}
	if cfg.DockerBin != "" {
		w.docker = newDockerCLI(cfg.DockerBin, cfg.WorkDir, cfg.Environ)
	}
	return w
}
