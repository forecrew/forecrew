package domain

// EnvironmentProvider is where a command job's environment comes from.
type EnvironmentProvider string

// Environment providers.
const (
	// ProviderDocker builds or pulls an image and runs the job in a
	// container on a worker with the docker label.
	ProviderDocker EnvironmentProvider = "docker"
	// ProviderMachine runs the job on the worker's host as it is.
	ProviderMachine EnvironmentProvider = "machine"
	// ProviderActions runs the job on a GitHub-hosted runner (pool actions).
	ProviderActions EnvironmentProvider = "actions"
)

// EnvironmentProviders returns every environment provider.
func EnvironmentProviders() []EnvironmentProvider {
	return []EnvironmentProvider{ProviderDocker, ProviderMachine, ProviderActions}
}

// Environment is a .forecrew/environments/<name>.yaml file
// (docs/contracts/environment.schema.json), resolved into the spec of a
// command job. It never holds secrets.
type Environment struct {
	Name     string              `json:"name"`
	Provider EnvironmentProvider `json:"provider"`
	// Docker: exactly one of Image, Dockerfile (with an optional Context)
	// or Devcontainer. Paths are relative to the repo root.
	Image        string `json:"image,omitempty"`
	Dockerfile   string `json:"dockerfile,omitempty"`
	Context      string `json:"context,omitempty"`
	Devcontainer string `json:"devcontainer,omitempty"`
	// RunsOn is the GitHub-hosted runner label (actions only), e.g.
	// ubuntu-24.04.
	RunsOn string `json:"runs_on,omitempty"`
	// Needs are worker labels, normalized like Job.Needs.
	Needs []string `json:"needs,omitempty"`
	// Setup commands run once when the image is built (docker), before the
	// job on a machine, or as workflow steps (actions).
	Setup []string `json:"setup,omitempty"`
	// Env holds non-secret variables.
	Env map[string]string `json:"env,omitempty"`
	// Versions maps a tool name to the command whose output is recorded as
	// its version, e.g. go -> "go version".
	Versions map[string]string `json:"versions,omitempty"`
	// Services run next to the job, e.g. a database.
	Services map[string]Service `json:"services,omitempty"`
}

// Service is a container started next to a command job.
type Service struct {
	Image  string            `json:"image"`
	Env    map[string]string `json:"env,omitempty"` // non-secret variables
	Ports  []int             `json:"ports,omitempty"`
	Health *Health           `json:"health,omitempty"`
}

// Health is how a service is known to be ready.
type Health struct {
	Command  string   `json:"command"`            // shell command run in the service container
	Interval Duration `json:"interval,omitempty"` // between tries; 0 means 2s
	Retries  int      `json:"retries,omitempty"`  // tries before giving up; 0 means 30
}

// Validate checks the rules the JSON schema cannot express clearly: the
// provider's fields, relative paths and secret-looking variable names. It
// reports every problem at once, wrapping ErrInvalidSpec.
func (e Environment) Validate() error {
	var p problems
	if !ValidName(e.Name) {
		p.addf("name %q must match %s", e.Name, nameRE)
	}
	e.checkProvider(&p)
	p.checkPaths("dockerfile", nonEmpty(e.Dockerfile))
	p.checkPaths("context", nonEmpty(e.Context))
	p.checkPaths("devcontainer", nonEmpty(e.Devcontainer))
	for _, s := range e.Setup {
		if s == "" {
			p.addf("setup: empty command")
		}
	}
	p.checkEnv("env", e.Env)
	for _, tool := range sortedKeys(e.Versions) {
		if tool == "" || e.Versions[tool] == "" {
			p.addf("versions: %q needs a tool name and a command", tool)
		}
	}
	for _, name := range sortedKeys(e.Services) {
		e.Services[name].check(name, &p)
	}
	return p.err("environment " + e.Name)
}

func (e Environment) checkProvider(p *problems) {
	sources := len(nonEmpty(e.Image)) + len(nonEmpty(e.Dockerfile)) + len(nonEmpty(e.Devcontainer))
	switch e.Provider {
	case ProviderDocker:
		if sources != 1 {
			p.addf("docker needs exactly one of image, dockerfile or devcontainer")
		}
		if e.Context != "" && e.Dockerfile == "" {
			p.addf("context needs dockerfile")
		}
		if e.RunsOn != "" {
			p.addf("runs_on is only for provider actions")
		}
	case ProviderMachine, ProviderActions:
		if sources > 0 || e.Context != "" {
			p.addf("image, dockerfile, context and devcontainer are only for provider docker")
		}
		if e.Provider == ProviderActions && e.RunsOn == "" {
			p.addf("actions needs runs_on, e.g. ubuntu-24.04")
		}
		if e.Provider == ProviderMachine && e.RunsOn != "" {
			p.addf("runs_on is only for provider actions")
		}
	default:
		p.addf("provider must be docker, machine or actions, got %q", e.Provider)
	}
}

func (s Service) check(name string, p *problems) {
	if !ValidName(name) {
		p.addf("services.%s: name must match %s", name, nameRE)
	}
	if s.Image == "" {
		p.addf("services.%s: image must be set", name)
	}
	p.checkEnv("services."+name+".env", s.Env)
	for _, port := range s.Ports {
		if port < 1 || port > 65535 {
			p.addf("services.%s: port %d out of range", name, port)
		}
	}
	if h := s.Health; h != nil && (h.Command == "" || h.Interval < 0 || h.Retries < 0) {
		p.addf("services.%s.health: command must be set, interval and retries not negative", name)
	}
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
