package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

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
// environ is the process environment (os.Environ()).
func newFromConfig(ctx context.Context, cfg *config.Worker, environ []string, version string, logger *slog.Logger) *Worker {
	labels, dockerBin, _ := detectLabels(ctx, cfg, environ)
	dockerHost := ""
	if dockerBin != "" && !cfg.RegistryAuth {
		dockerHost = newDockerCLI(dockerBin, cfg.WorkDir, environ).host(ctx)
	}
	return New(Config{
		RegistryAuth: cfg.RegistryAuth, DockerHost: dockerHost,
		DockerBin: dockerBin, ImageMaxAge: time.Duration(cfg.ImagePruneDays) * 24 * time.Hour,
		Client: NewClient(cfg.ServerURL, cfg.Token, nil), Labels: labels, Version: version,
		WorkDir: cfg.WorkDir, MaxJobs: cfg.MaxJobs,
		Runtimes: runtime.DefaultRegistry(AgentEnv(environ), nil), Analyzer: analyzer.Local{},
		Logger: logger, Environ: environ,
	})
}

// Run runs the worker of cfg until ctx is done or the server rejects its
// token. environ is the process environment (os.Environ()).
func Run(ctx context.Context, cfg *config.Worker, environ []string, version string, logger *slog.Logger) error {
	if _, err := newGitClient("git", nil, environ, "").version(ctx); err != nil {
		return fmt.Errorf("git is required: %w", err)
	}
	w := newFromConfig(ctx, cfg, environ, version, logger)
	logger.Info("forecrew-worker started", "version", version, "name", cfg.Name, "server_url", cfg.ServerURL,
		"labels", w.cfg.Labels, "max_jobs", cfg.MaxJobs, "work_dir", cfg.WorkDir)
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

// ErrCheckFailed means Check found a problem; the details are in its output.
var ErrCheckFailed = errors.New("check failed")

// Check prints the labels of the worker and verifies git, the runtime CLIs
// and the connection to the server with the worker token. A missing runtime
// CLI is reported but is not a failure; a missing git or an unreachable
// server is.
func Check(ctx context.Context, cfg *config.Worker, environ []string, version string, out io.Writer) error {
	labels, dockerBin, dockerInfo := detectLabels(ctx, cfg, environ)
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
// engine answers, and what detectDocker found.
func detectLabels(ctx context.Context, cfg *config.Worker, environ []string) (labels []string, dockerBin, dockerInfo string) {
	dockerBin, dockerInfo = detectDocker(ctx, cfg.WorkDir, environ)
	configured := cfg.Labels
	if dockerBin != "" {
		configured = append(slices.Clone(configured), dockerLabel)
	}
	return DetectLabels(configured, goruntime.GOOS, exec.LookPath), dockerBin, dockerInfo
}

// detectDocker returns the docker client binary when the Docker engine
// answers `docker version`, and the answer or the reason it did not.
func detectDocker(ctx context.Context, workDir string, environ []string) (bin, info string) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	v, err := newDockerCLI("docker", workDir, environ).version(ctx)
	if err != nil {
		return "", err.Error()
	}
	return "docker", v
}
