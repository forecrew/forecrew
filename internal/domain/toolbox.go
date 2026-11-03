package domain

// Toolbox is a packs/toolboxes/<name>.yaml file
// (docs/contracts/toolbox.schema.json): the image an agent job runs in, with
// the CLIs and MCP servers it offers. It is resolved into the spec of agent
// jobs whose role names it. It never holds secrets.
type Toolbox struct {
	Name string `json:"name"`
	// Exactly one of Image and Dockerfile; Dockerfile is relative to
	// packs/toolboxes and built with that directory as context.
	Image      string `json:"image,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// Needs are worker labels besides docker, which is implied.
	Needs      []string    `json:"needs,omitempty"`
	CLIs       []ToolCLI   `json:"clis,omitempty"`
	MCPServers []MCPServer `json:"mcp_servers,omitempty"`
}

// ToolCLI is a command-line tool a toolbox provides.
type ToolCLI struct {
	Name    string `json:"name"`
	Version string `json:"version"` // command printing its version, e.g. "semgrep --version"
}

// MCPServer is a stdio MCP server the agent CLI starts inside the toolbox.
type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"` // non-secret variables
	// CredentialEnv names variables of the job's env (its credentials)
	// passed to the server; secrets only ever arrive this way.
	CredentialEnv []string `json:"credential_env,omitempty"`
}

// Validate checks names, the image source and secret-looking variable
// names, reporting every problem at once and wrapping ErrInvalidSpec.
func (t Toolbox) Validate() error {
	var p problems
	if !ValidName(t.Name) {
		p.addf("name %q must match %s", t.Name, nameRE)
	}
	if (t.Image == "") == (t.Dockerfile == "") {
		p.addf("exactly one of image and dockerfile must be set")
	}
	p.checkPaths("dockerfile", nonEmpty(t.Dockerfile))
	seen := map[string]bool{}
	for _, c := range t.CLIs {
		if !ValidName(c.Name) || seen["cli:"+c.Name] {
			p.addf("clis: name %q must be unique and match %s", c.Name, nameRE)
		}
		seen["cli:"+c.Name] = true
		if c.Version == "" {
			p.addf("clis.%s: version command must be set", c.Name)
		}
	}
	for _, s := range t.MCPServers {
		if !ValidName(s.Name) || seen["mcp:"+s.Name] {
			p.addf("mcp_servers: name %q must be unique and match %s", s.Name, nameRE)
		}
		seen["mcp:"+s.Name] = true
		if s.Command == "" {
			p.addf("mcp_servers.%s: command must be set", s.Name)
		}
		p.checkEnv("mcp_servers."+s.Name+".env", s.Env)
		for _, c := range s.CredentialEnv {
			if c == "" {
				p.addf("mcp_servers.%s.credential_env: empty name", s.Name)
			}
		}
	}
	return p.err("toolbox " + t.Name)
}
