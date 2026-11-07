package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

// Opencode is the adapter name of the opencode CLI.
const Opencode = "opencode"

// OpencodeVersion is the only opencode release the adapter runs. What keeps a
// checked-out repo from widening a job (see opencodeEnv) was read in the
// source of this release, and neither the event stream nor the variables are
// a stable interface, so a change of version needs that check again.
const OpencodeVersion = "1.19.5"

// opencodeMCPTimeout bounds one gateway request; opencode applies it to the
// call itself, so it stays above the gateway's own limit.
const opencodeMCPTimeout = jobMCPToolTimeout + time.Minute

// opencodeKeepEnv is the OPENCODE_ variable that is a credential, not a
// setting: the key of opencode's own provider.
const opencodeKeepEnv = "OPENCODE_API_KEY" // #nosec G101 -- a variable name, not a secret

// opencodePermissions maps the Claude Code tool names of AllowedTools to
// opencode's permission keys. Bash is handled apart, because of its patterns.
var opencodePermissions = map[string][]string{
	"Read":         {"read", "list"},
	"LS":           {"list"},
	"Glob":         {"glob"},
	"Grep":         {"grep"},
	"Edit":         {"edit"},
	"MultiEdit":    {"edit"},
	"Write":        {"edit"},
	"NotebookEdit": {"edit"},
	"TodoWrite":    {"todowrite"},
	"WebFetch":     {"webfetch"},
	"WebSearch":    {"websearch"},
}

// opencodeCLI runs `opencode run` non-interactively.
type opencodeCLI struct{ cfg Config }

// NewOpencode returns the opencode adapter.
func NewOpencode(cfg Config) Adapter { return &opencodeCLI{cfg: cfg} }

func (c *opencodeCLI) Name() string { return Opencode }

func (c *opencodeCLI) Detect(ctx context.Context) (string, error) {
	return detectVersion(ctx, c.cfg, "opencode")
}

// OpencodeRules returns the opencode permission rules of allowed tools (the
// form of RunSpec.AllowedTools; none means read-only) and the scratch
// directory, empty for none. Everything starts denied, so no rule is ever
// `ask`, which nobody could answer. opencode's last matching rule wins: the
// JSON encoder sorts keys, which puts "*" first, and a pattern sorted before
// it would only lose its allow. A tool without a mapping is an error, never
// an allow.
func OpencodeRules(allowed []string, evidenceDir string) (map[string]any, error) {
	if len(allowed) == 0 {
		allowed = readOnlyClaudeTools
	}
	external := map[string]string{"*": "deny"}
	if evidenceDir != "" {
		external[filepath.ToSlash(evidenceDir)+"/*"] = "allow"
	}
	rules := map[string]any{"*": "deny", "doom_loop": "deny", "external_directory": external}
	shell := map[string]string{"*": "deny"}
	for _, tool := range allowed {
		if name, ok := JobMCPToolOf(tool); ok {
			rules[opencodeMCPTool(name)] = "allow"
			continue
		}
		pattern, isShell, err := opencodeShellPattern(tool)
		if err != nil {
			return nil, err
		}
		if isShell {
			shell[pattern] = "allow"
			continue
		}
		keys, ok := opencodePermissions[tool]
		if !ok {
			return nil, fmt.Errorf("allowed tool %q has no opencode permission", tool)
		}
		for _, k := range keys {
			rules[k] = "allow"
		}
	}
	if shell["*"] == "allow" {
		rules["bash"] = "allow"
	} else if len(shell) > 1 {
		rules["bash"] = shell
	}
	return rules, nil
}

// opencodeShellPattern returns the opencode pattern of a Bash rule: Bash is
// every command, Bash(git log *) the pattern inside, and the older form
// Bash(go test:*) is the same as Bash(go test *). Braces are refused, because
// opencode expands {env:...} and {file:...} in the inline configuration.
func opencodeShellPattern(tool string) (pattern string, isShell bool, err error) {
	if tool == "Bash" {
		return "*", true, nil
	}
	inner, ok := strings.CutPrefix(tool, "Bash(")
	if !ok {
		return "", false, nil
	}
	inner, ok = strings.CutSuffix(inner, ")")
	if !ok || strings.TrimSpace(inner) == "" || strings.ContainsAny(inner, "{}") {
		return "", false, fmt.Errorf("allowed tool %q has no opencode permission", tool)
	}
	if prefix, ok := strings.CutSuffix(inner, ":*"); ok {
		inner = prefix + " *"
	}
	return inner, true, nil
}

// opencodeMCPTool returns opencode's name of a gateway tool: the server name,
// an underscore and the tool.
func opencodeMCPTool(tool string) string { return JobMCPServer + "_" + tool }

// opencodeToolName returns the name an event reports for an opencode tool:
// gateway tools take the form of JobMCPTool, as in the role's allowed tools.
func opencodeToolName(tool string) string {
	if name, ok := strings.CutPrefix(tool, JobMCPServer+"_"); ok && name != "" {
		return JobMCPTool(name)
	}
	return tool
}

// opencodeConfig returns the inline configuration of a run. Besides the
// permissions and the job MCP gateway it turns off what would run or fetch
// something the job was not given: formatters and language servers run
// binaries of the checkout, even for a role without a shell. Only the
// model's provider is enabled, so other keys in the environment stay unused.
// The model is declared so that one newer than opencode's catalog still
// resolves.
func opencodeConfig(spec RunSpec, rules map[string]any) (string, error) {
	provider, model, ok := domain.SplitProviderModel(spec.Model)
	if !ok {
		return "", fmt.Errorf("model %q is not in provider/model form", spec.Model)
	}
	cfg := map[string]any{
		"autoupdate": false, "share": "disabled", "snapshot": false, "formatter": false, "lsp": false,
		"plugin": []string{}, "instructions": []string{}, "enabled_providers": []string{provider},
		"model": spec.Model, "small_model": spec.Model, "default_agent": "build",
		"permission": rules, "provider": map[string]any{provider: opencodeProvider(model, spec.Endpoint)},
	}
	if spec.MaxTurns > 0 {
		// After this many steps opencode takes the tools away and asks for
		// a final answer.
		cfg["agent"] = map[string]any{"build": map[string]any{"steps": spec.MaxTurns}}
	}
	if spec.JobMCP != nil {
		cfg["mcp"] = map[string]any{JobMCPServer: map[string]any{
			"type": "remote", "url": spec.JobMCP.URL, "enabled": true, "oauth": false,
			"headers": map[string]string{"Authorization": "Bearer {env:" + JobMCPTokenEnv + "}"},
			"timeout": opencodeMCPTimeout.Milliseconds(),
		}}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("opencode config: %w", err)
	}
	return string(b), nil
}

// opencodeProvider returns the provider entry of the model. A custom endpoint
// is an OpenAI-compatible provider whose key is read from its variable, so
// the key is in no argument and no file.
func opencodeProvider(model string, endpoint *domain.Endpoint) map[string]any {
	p := map[string]any{"models": map[string]any{model: map[string]any{}}}
	if endpoint == nil {
		return p
	}
	options := map[string]string{"baseURL": endpoint.BaseURL}
	if endpoint.KeyEnv != "" {
		options["apiKey"] = "{env:" + endpoint.KeyEnv + "}"
	}
	p["npm"], p["options"] = "@ai-sdk/openai-compatible", options
	return p
}

// opencodeEnv is what isolates a run, with root its private directory.
//
// The XDG directories and OPENCODE_TEST_HOME move everything opencode reads
// from the worker account (config, login, sessions, ~/.opencode, ~/.claude)
// to root. OPENCODE_TEST_HOME is an internal variable, but HOME must stay: the
// agent's shell needs the account's toolchains. OPENCODE_DISABLE_PROJECT_CONFIG
// is what keeps the checkout out: without it the checkout's opencode.json and
// .opencode/ (permissions, plugins, tools, agents) merge with the inline
// configuration, which only wins key by key. It also drops AGENTS.md, which
// the role's prompt does not rely on.
func opencodeEnv(root, workDir, config string) []string {
	return []string{
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"OPENCODE_TEST_HOME=" + filepath.Join(root, "home"),
		"OPENCODE_CONFIG_CONTENT=" + config,
		"OPENCODE_CONFIG=", "OPENCODE_CONFIG_DIR=", "OPENCODE_PERMISSION=",
		"OPENCODE_DISABLE_PROJECT_CONFIG=true",
		"OPENCODE_DISABLE_CLAUDE_CODE=true",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=true",
		"OPENCODE_PURE=true",
		"OPENCODE_DISABLE_AUTOUPDATE=true",
		"OPENCODE_DISABLE_MODELS_FETCH=true",
		"OPENCODE_DISABLE_LSP_DOWNLOAD=true",
		// opencode takes its directory from PWD, which exec leaves as the
		// worker's own when it is given an environment.
		"PWD=" + workDir,
	}
}

// opencodeArgs builds the argument list. The prompt goes through stdin. A
// non-empty session continues that session.
func opencodeArgs(spec RunSpec, session string) []string {
	args := []string{"run", "--format", "json", "--model", spec.Model}
	if spec.WorkDir != "" {
		args = append(args, "--dir", spec.WorkDir)
	}
	if session != "" {
		args = append(args, "--session", session)
	}
	return args
}

// withoutOpencodeEnv removes the worker's own OPENCODE_ variables, which
// could point opencode at other configuration, a database or a model catalog.
func withoutOpencodeEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "OPENCODE_") || name == opencodeKeepEnv {
			out = append(out, kv)
		}
	}
	return out
}

// withEndpointKey makes sure a custom endpoint gets its key: a key from the
// worker's environment joins spec.Env, so it is redacted like the others, and
// a job without one fails now rather than at the provider. The endpoint of an
// organization's connection takes only the key the job came with.
func withEndpointKey(spec RunSpec, base []string) (RunSpec, error) {
	if spec.Endpoint == nil || spec.Endpoint.KeyEnv == "" || spec.Env[spec.Endpoint.KeyEnv] != "" {
		return spec, nil
	}
	name := spec.Endpoint.KeyEnv
	if spec.Endpoint.Organization {
		return spec, fmt.Errorf("%w: the job has no %s, and a key of this worker does not go to an organization's endpoint", ErrRunFailed, name)
	}
	i := slices.IndexFunc(base, func(kv string) bool {
		v, ok := strings.CutPrefix(kv, name+"=")
		return ok && v != ""
	})
	if i < 0 {
		return spec, fmt.Errorf("%w: %s is not set: add it to the runtime's credential_env or the worker's environment", ErrRunFailed, name)
	}
	env := maps.Clone(spec.Env)
	if env == nil {
		env = map[string]string{}
	}
	env[name] = strings.TrimPrefix(base[i], name+"=")
	spec.Env = env
	return spec, nil
}

func (c *opencodeCLI) Run(ctx context.Context, spec RunSpec, emit func(domain.JobEvent)) (Result, error) {
	version, err := c.Detect(ctx)
	if err != nil {
		return Result{}, err
	}
	if version != OpencodeVersion {
		return Result{}, fmt.Errorf("%w: opencode %s is not supported: install %s", ErrRunFailed, version, OpencodeVersion)
	}
	rules, err := OpencodeRules(spec.AllowedTools, spec.EvidenceDir)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrRunFailed, err)
	}
	base := startEnv(c.cfg.Env, nil)
	if spec, err = withEndpointKey(spec, base); err != nil {
		return Result{}, err
	}
	root, err := os.MkdirTemp("", "forecrew-opencode-")
	if err != nil {
		return Result{}, fmt.Errorf("create opencode dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := os.Mkdir(filepath.Join(root, "home"), 0o700); err != nil {
		return Result{}, fmt.Errorf("create opencode dir: %w", err)
	}
	r := opencodeRun{a: c, cfg: c.cfg, rules: rules, root: root, emit: emit, parser: &opencodeParser{}}
	r.cfg.Env = withoutOpencodeEnv(base)
	start := time.Now()
	res, err := r.attempt(ctx, spec, "", spec.Prompt)
	invalid, ok := errors.AsType[*InvalidOutputError](err)
	if !ok || r.parser.session == "" {
		return res, err
	}
	retry := spec
	if spec.Timeout > 0 {
		if retry.Timeout = spec.Timeout - time.Since(start); retry.Timeout <= 0 {
			return res, err
		}
	}
	if spec.MaxTurns > 0 {
		retry.MaxTurns = max(spec.MaxTurns-res.Turns, 1)
	}
	r.parser.text = ""
	return r.attempt(ctx, retry, r.parser.session, opencodeRetryPrompt(invalid.Message, spec.OutputSchema))
}

// opencodeRetryPrompt asks for the output again. opencode cannot constrain
// its output to a schema, so the schema goes in the words of the prompt.
func opencodeRetryPrompt(problem string, schema json.RawMessage) string {
	return "Your final message failed validation: " + problem +
		"\n\nAnswer again. End your final message with one fenced ```json block that matches this JSON Schema:\n\n```json\n" +
		string(schema) + "\n```\n"
}

// opencodeRun is one job: one attempt, and a second one in the same session
// when the output is invalid. Both share the directory that holds the
// session, and the parser, so turns and usage add up.
type opencodeRun struct {
	a      *opencodeCLI
	cfg    Config
	rules  map[string]any
	root   string
	emit   func(domain.JobEvent)
	parser *opencodeParser
}

func (r *opencodeRun) attempt(ctx context.Context, spec RunSpec, session, prompt string) (Result, error) {
	config, err := opencodeConfig(spec, r.rules)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrRunFailed, err)
	}
	cli := cliRun{
		binary: r.a.cfg.binary("opencode"), args: opencodeArgs(spec, session), stdin: prompt,
		env: opencodeEnv(r.root, spec.WorkDir, config),
	}
	return runCLI(ctx, r.cfg, spec, cli, r.parser, r.emit)
}

// opencodeParser reads the JSON events of `opencode run --format json`.
type opencodeParser struct {
	session string
	text    string
	turns   int
	usage   domain.Usage
	costUSD float64
	failure string
}

type opencodeEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      opencodePart    `json:"part"`
	Error     json.RawMessage `json:"error"`
}

type opencodePart struct {
	Text   string  `json:"text"`
	Tool   string  `json:"tool"`
	Cost   float64 `json:"cost"`
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	State struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output string          `json:"output"`
		Error  string          `json:"error"`
	} `json:"state"`
}

func (p *opencodeParser) line(raw []byte, e *emitter) {
	var ev opencodeEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		e.event(domain.EventLog, "unparseable output line: "+truncate(string(raw), 200), nil)
		return
	}
	if p.session == "" && ev.SessionID != "" {
		p.session = ev.SessionID
		e.event(domain.EventStatus, "session started", nil)
	}
	switch ev.Type {
	case "step_start":
		p.turns++
	case "step_finish":
		// Each step reports its own tokens. Input leaves out the cache, and
		// output leaves out the reasoning, which is billed as output.
		t := ev.Part.Tokens
		p.usage.InputTokens += t.Input
		p.usage.OutputTokens += t.Output + t.Reasoning
		p.usage.CacheReadTokens += t.Cache.Read
		p.usage.CacheWriteTokens += t.Cache.Write
		p.costUSD += ev.Part.Cost
	case "text":
		p.text = ev.Part.Text
		e.event(domain.EventMessage, ev.Part.Text, nil)
	case "tool_use":
		// opencode reports a call once, when it is over.
		name := opencodeToolName(ev.Part.Tool)
		failed := ev.Part.State.Status == "error"
		e.event(domain.EventToolCall, name, map[string]any{"name": name, "input": ev.Part.State.Input})
		e.event(domain.EventToolResult, name, map[string]any{
			"name": name, "output": truncate(cmp.Or(ev.Part.State.Error, ev.Part.State.Output), maxToolOutput), "is_error": failed,
		})
	case "error":
		p.failure = opencodeError(ev.Error)
	}
}

// opencodeError returns the message of an error event: an object with a name
// and data.message, or anything else a failed request left there.
func opencodeError(raw json.RawMessage) string {
	var e struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && (e.Data.Message != "" || e.Name != "") {
		return cmp.Or(e.Data.Message, e.Name)
	}
	return cmp.Or(truncate(string(raw), stderrTail), "opencode reported an error")
}

func (p *opencodeParser) done() outcome {
	return outcome{
		text: p.text, turns: p.turns, usage: p.usage, costUSD: p.costUSD, failure: p.failure,
		incomplete: p.turns == 0,
	}
}
