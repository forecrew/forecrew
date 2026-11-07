package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// EnvWorkerToken holds the worker token printed by `forecrew worker add`.
const EnvWorkerToken = "FORECREW_WORKER_TOKEN"

// EnvBootstrapToken holds the one-time token of a machine's worker, in place
// of a worker token (fleet.md, Bootstrap tokens).
const EnvBootstrapToken = "FORECREW_BOOTSTRAP_TOKEN" // #nosec G101 -- the name of a variable

// DefaultMaxJobs is the number of jobs a worker runs at once by default.
const DefaultMaxJobs = 1

// DefaultImagePruneDays is how long an environment image stays on a worker
// after its last use by default.
const DefaultImagePruneDays = 14

// Worker is forecrew-worker's configuration (worker.yaml).
type Worker struct {
	ServerURL string `yaml:"server_url"` // forecrewd public_base_url
	Name      string `yaml:"name"`       // defaults to the host name
	// Labels are toolchains and anything else jobs may need. The worker adds
	// its os label (linux, darwin, windows) and runtime:<name> for each
	// runtime CLI it finds.
	Labels  []string `yaml:"labels"`
	WorkDir string   `yaml:"work_dir"` // defaults to <user cache dir>/forecrew-worker
	MaxJobs int      `yaml:"max_jobs"`
	// ImagePruneDays removes the Docker images built for environments
	// (forecrew-env:*) that no job used for that many days.
	ImagePruneDays int `yaml:"image_prune_days"`
	// RegistryAuth lets Docker pulls and builds use the registry logins of
	// the worker's user. Off by default: image names come from repos.
	RegistryAuth bool `yaml:"registry_auth"`
	// LocalModelEndpoints lets the key check of a model connection reach
	// localhost and private addresses, where a self-hosted model (Ollama,
	// LM Studio) runs. Off, it reaches public addresses only; link-local
	// addresses, where cloud metadata lives, never.
	LocalModelEndpoints bool `yaml:"local_model_endpoints"`
	// AutoInstallAgents lets the worker install an agent CLI it lacks when
	// forecrewd asks, from the manifest of its own release, into its own
	// directory. Nil means true; a machine of the fleet never installs.
	AutoInstallAgents *bool `yaml:"auto_install_agents"`
	// Token comes from FORECREW_WORKER_TOKEN.
	Token string `yaml:"-"`
	// BootstrapToken comes from FORECREW_BOOTSTRAP_TOKEN. The worker trades
	// it for Token, its name and its labels at start.
	BootstrapToken string `yaml:"-"`
}

// LoadWorker reads worker.yaml from path.
func LoadWorker(path string, getenv func(string) string) (*Worker, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's config file
	if err != nil {
		return nil, fmt.Errorf("read worker config: %w", err)
	}
	return ParseWorker(data, getenv)
}

// ParseWorker decodes and validates worker.yaml, applies defaults and reads
// the token with getenv.
func ParseWorker(data []byte, getenv func(string) string) (*Worker, error) {
	var w Worker
	if err := decodeStrict(data, &w); err != nil {
		return nil, err
	}
	w.Token = getenv(EnvWorkerToken)
	w.BootstrapToken = getenv(EnvBootstrapToken)
	if err := w.applyDefaults(); err != nil {
		return nil, err
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

func (w *Worker) applyDefaults() error {
	if w.MaxJobs == 0 {
		w.MaxJobs = DefaultMaxJobs
	}
	if w.ImagePruneDays == 0 {
		w.ImagePruneDays = DefaultImagePruneDays
	}
	if w.Name == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("default worker name: %w", err)
		}
		w.Name = host
	}
	if w.WorkDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("default work_dir: %w", err)
		}
		w.WorkDir = filepath.Join(cache, "forecrew-worker")
	}
	return nil
}

// InstallsAgents reports whether auto_install_agents is on (the default).
func (w *Worker) InstallsAgents() bool { return w.AutoInstallAgents == nil || *w.AutoInstallAgents }

// Validate checks the worker configuration and reports every problem at once.
func (w *Worker) Validate() error {
	var p problems
	u, err := url.Parse(w.ServerURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		p.addf("server_url must be an absolute http(s) URL, got %q", w.ServerURL)
	}
	if w.MaxJobs < 1 {
		p.addf("max_jobs must be at least 1, got %d", w.MaxJobs)
	}
	if w.ImagePruneDays < 1 {
		p.addf("image_prune_days must be at least 1, got %d", w.ImagePruneDays)
	}
	if (w.Token == "") == (w.BootstrapToken == "") {
		p.addf("exactly one of %s and %s must be set", EnvWorkerToken, EnvBootstrapToken)
	}
	return p.err()
}
