package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/runtime"
)

// checkTimeout bounds each step of Check.
const checkTimeout = 15 * time.Second

// AgentEnv returns the environment the agent processes inherit: the worker's
// own, without FORECREW_ variables (the worker token) and GIT_ variables
// (they could point the agent's git at another repo or turn on tracing).
func AgentEnv(environ []string) []string {
	return filterEnv(environ, func(name string) bool {
		return !strings.HasPrefix(name, "FORECREW_") && !strings.HasPrefix(name, "GIT_")
	})
}

// filterEnv returns the entries of environ whose variable name keep accepts.
func filterEnv(environ []string, keep func(name string) bool) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if keep(name) {
			out = append(out, kv)
		}
	}
	return out
}

// newFromConfig builds the Worker of a worker.yaml: labels from the config and the
// machine, the runtime adapters found on PATH and the local analyzer.
// environ is the process environment (os.Environ()); fleet marks the worker
// of a fleet machine, which uses the agents of its image.
func newFromConfig(ctx context.Context, cfg *config.Worker, environ []string, version string, logger *slog.Logger, fleet bool) *Worker {
	var agents *agentInstaller
	if cfg.InstallsAgents() && !fleet {
		agents = setUpAgents(cfg.WorkDir, logger)
	}
	look := exec.LookPath
	if agents != nil {
		look = agents.lookPath
		environ = prependPath(environ, agents.binDir())
	}
	labels, dockerBin, _ := detectLabels(ctx, cfg, environ, look)
	dockerHost := ""
	if dockerBin != "" && !cfg.RegistryAuth {
		dockerHost = newDockerCLI(dockerBin, cfg.WorkDir, environ).host(ctx)
	}
	toolboxes, _ := fs.Sub(forecrew.Files, "packs/toolboxes") // the path exists in the embedded files
	return New(Config{
		Toolboxes: toolboxes, RegistryAuth: cfg.RegistryAuth, DockerHost: dockerHost, LocalModelEndpoints: cfg.LocalModelEndpoints,
		DockerBin: dockerBin, ImageMaxAge: time.Duration(cfg.ImagePruneDays) * 24 * time.Hour,
		Client: NewClient(cfg.ServerURL, cfg.Token, nil), Labels: labels, Version: version,
		WorkDir: cfg.WorkDir, MaxJobs: cfg.MaxJobs,
		Runtimes: runtime.DefaultRegistry(AgentEnv(environ), nil), Analyzer: analyzer.Local{},
		Logger: logger, Environ: environ, agents: agents, fleet: fleet,
	})
}

// setUpAgents returns the installer of the worker's agents directory, or nil
// when the worker cannot install: as root it never does, so nothing it
// downloads runs with elevated rights.
func setUpAgents(workDir string, logger *slog.Logger) *agentInstaller {
	if uid, _ := hostIDs(); uid == 0 {
		logger.Warn("agents are not installed automatically: the worker runs as root")
		return nil
	}
	m, err := DefaultAgentManifest()
	if err != nil {
		logger.Error("agents are not installed automatically: read the manifest", "error", err)
		return nil
	}
	a := newAgentInstaller(agentsDir(workDir), m, goruntime.GOOS, goruntime.GOARCH, nil, nil)
	if err := a.prepare(); err != nil {
		logger.Error("agents are not installed automatically: prepare the agents directory", "error", err)
		return nil
	}
	return a
}

// Run runs the worker of cfg until ctx is done or the server rejects its
// token. environ is the process environment (os.Environ()).
func Run(ctx context.Context, cfg *config.Worker, environ []string, version string, logger *slog.Logger) error {
	if _, err := newGitClient("git", nil, environ, "").version(ctx); err != nil {
		return fmt.Errorf("git is required: %w", err)
	}
	fleet := cfg.BootstrapToken != ""
	if fleet {
		if err := bootstrap(ctx, cfg, version, logger); err != nil {
			return err
		}
	}
	w := newFromConfig(ctx, cfg, environ, version, logger, fleet)
	if w.cfg.agents != nil {
		// The runtime adapters look their CLI up on the process's PATH.
		if err := os.Setenv("PATH", w.cfg.agents.binDir()+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
			return err
		}
	}
	logger.Info("forecrew-worker started", "version", version, "name", cfg.Name, "server_url", cfg.ServerURL,
		"labels", w.cfg.Labels, "auto_install_agents", w.cfg.agents != nil, "max_jobs", cfg.MaxJobs, "work_dir", cfg.WorkDir)
	if cfg.MaxJobs > 1 {
		logger.Warn("max_jobs is above 1: concurrent jobs share this user, so their agents can read each other's repo tokens; use it only for trusted repos and roles",
			"max_jobs", cfg.MaxJobs)
	}
	err := w.Run(ctx)
	if err == nil {
		logger.Info("forecrew-worker stopped")
	}
	return err
}

// bootstrap trades the machine's one-time token for what the worker needs,
// which stays in memory: a worker that restarts cannot come back, and its
// machine is replaced.
func bootstrap(ctx context.Context, cfg *config.Worker, version string, logger *slog.Logger) error {
	logger.Info("forecrew-worker bootstrapping", "version", version, "server_url", cfg.ServerURL)
	g, err := NewClient(cfg.ServerURL, cfg.BootstrapToken, nil).Bootstrap(ctx, version)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	cfg.Token, cfg.BootstrapToken = g.WorkerToken, ""
	cfg.Name, cfg.MaxJobs = g.Name, g.MaxJobs
	cfg.Labels = append(cfg.Labels, g.Labels...)
	return nil
}

// ErrCheckFailed means Check found a problem; the details are in its output.
var ErrCheckFailed = errors.New("check failed")

// Check prints the labels of the worker and verifies git, the runtime CLIs
// and the connection to the server with the worker token. A missing runtime
// CLI is reported but is not a failure; a missing git or an unreachable
// server is.
func Check(ctx context.Context, cfg *config.Worker, environ []string, version string, out io.Writer) error {
	look := exec.LookPath
	if cfg.InstallsAgents() {
		dir := agentsDir(cfg.WorkDir)
		look = (&agentInstaller{dir: dir, goos: goruntime.GOOS}).lookPath
		environ = prependPath(environ, filepath.Join(dir, "bin"))
	}
	labels, dockerBin, dockerInfo := detectLabels(ctx, cfg, environ, look)
	fmt.Fprintf(out, "forecrew-worker %s\nlabels: %s\n", version, strings.Join(labels, ", "))
	if dockerBin != "" {
		fmt.Fprintf(out, "docker: engine %s\n", dockerInfo)
	} else {
		fmt.Fprintf(out, "docker: not available (%s)\n", dockerInfo)
	}
	failed := false

	stepCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	gitVersion, err := newGitClient("git", nil, environ, "").version(stepCtx)
	cancel()
	if err != nil {
		fmt.Fprintf(out, "git: FAILED: %v\n", err)
		failed = true
	} else {
		fmt.Fprintf(out, "git: %s\n", strings.TrimSpace(gitVersion))
	}

	reg := runtime.DefaultRegistry(AgentEnv(environ), nil)
	for _, cli := range runtimeCLIs {
		adapter, _ := reg.Get(cli.adapter)
		stepCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		v, err := adapter.Detect(stepCtx)
		cancel()
		if err != nil {
			fmt.Fprintf(out, "runtime %s: not available (%v)\n", cli.adapter, err)
			continue
		}
		fmt.Fprintf(out, "runtime %s: %s\n", cli.adapter, v)
	}

	// A poll without free slots only records that the worker is alive.
	stepCtx, cancel = context.WithTimeout(ctx, checkTimeout)
	_, err = NewClient(cfg.ServerURL, cfg.Token, nil).Poll(stepCtx, labels, 0, version)
	cancel()
	switch {
	case errors.Is(err, ErrUnauthorized):
		fmt.Fprintf(out, "server %s: reachable, but the worker token was rejected (revoked or wrong)\n", cfg.ServerURL)
		failed = true
	case err != nil:
		fmt.Fprintf(out, "server %s: FAILED: %v\n", cfg.ServerURL, err)
		failed = true
	default:
		fmt.Fprintf(out, "server %s: reachable, token accepted\n", cfg.ServerURL)
	}
	if failed {
		return ErrCheckFailed
	}
	return nil
}

// detectLabels returns the labels of the worker of cfg, with docker when the
// engine answers, and what detectDocker found. lookPath finds the runtime
// CLIs.
func detectLabels(ctx context.Context, cfg *config.Worker, environ []string, lookPath func(string) (string, error)) (labels []string, dockerBin, dockerInfo string) {
	dockerBin, dockerInfo = detectDocker(ctx, cfg.WorkDir, environ)
	configured := cfg.Labels
	if dockerBin != "" {
		configured = append(slices.Clone(configured), dockerLabel)
	}
	return DetectLabels(configured, goruntime.GOOS, lookPath), dockerBin, dockerInfo
}

// detectDocker returns the docker client binary when the Docker engine
// answers `docker version`, and the answer or the reason it did not.
func detectDocker(ctx context.Context, workDir string, environ []string) (bin, info string) {
	if uid, _ := hostIDs(); uid == 0 {
		return "", "the worker runs as root, which Docker environments refuse"
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	v, err := newDockerCLI("docker", workDir, environ).version(ctx)
	if err != nil {
		return "", err.Error()
	}
	return "docker", v
}
