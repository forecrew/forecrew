# Runtime adapters

`internal/runtime` runs an agent CLI for one job and turns its output into the
same events and result for every runtime. Adapters are looked up by name in a
`runtime.Registry`: `claude-code`, `codex`, `opencode` and `scripted`.

## What every adapter does

- Runs the CLI in `RunSpec.WorkDir` with the prompt on **stdin** (never in
  argv) and `RunSpec.Env` added to the process environment (credentials never
  go through arguments or files). Output still in the pipe when the CLI
  exits is read in full, however slow the `emit` callback is.
- Streams stdout line by line and emits normalized `domain.JobEvent`s:
  `status`, `message`, `tool_call`, `tool_result`, `usage`, `log` (an
  unparseable line) and `error`.
- Enforces `RunSpec.Timeout` and context cancellation by killing the whole
  process group (SIGKILL). Whatever the CLI left running (a dev server the
  agent started) is killed when the run ends, and its open pipes never hold
  the run up: output is read for at most 5 s after the group is stopped,
  even from a child that left the group and keeps writing. Timeout returns `ErrTimeout`, cancellation returns
  the context error, a non-zero exit or an error result returns
  `ErrRunFailed` with the reason and the stderr tail, a missing CLI returns
  `ErrNotInstalled`.
- Redacts secrets (values of `RunSpec.Env`, `sk-...`, GitHub and Forecrew
  tokens, bearer tokens, private keys) from every event, the result text, the
  structured output (extracted from the redacted text) and every error.
- Structured output: when `RunSpec.OutputSchema` is set, the last fenced
  `json` block of the final message (or the whole message if it is JSON) is
  validated against the inline schema (JSON Schema 2020-12). A missing,
  unparseable or non-matching block returns `*InvalidOutputError` (matches
  `ErrInvalidOutput`); its `Message` is the validator message, ready for a
  retry prompt. The result still carries text and usage in that case.
  The prompt (T10) must tell the agent to end with that block.

## claude-code

Written against Claude Code **2.1.309** (`claude --version`).

```
claude -p --output-format stream-json --verbose --no-session-persistence \
  --permission-mode dontAsk --setting-sources user \
  --tools <base names> --disallowedTools "mcp__*" \
  [--model <model>] [--max-turns <n>] --allowedTools <rule> [<rule> ...]
```

With a job MCP gateway (below), `--disallowedTools "mcp__*"` is replaced by
`--mcp-config <json> --strict-mcp-config`.

- `AllowedTools` is a hard limit. `--tools` limits which tools exist (the
  base names of the rules; `Bash(git log *)` gives `Bash`); `--allowedTools`
  only pre-approves, so it carries the rules themselves; `--permission-mode
  dontAsk` denies everything not pre-approved instead of prompting; MCP tools
  are disabled; `--setting-sources user` keeps repo and local settings out.
- Each run sets `CLAUDE_CONFIG_DIR` to a fresh empty temporary directory,
  removed afterwards. This stops the worker account's own `~/.claude` (allow
  rules, hooks) from widening permissions, and means no login state is used:
  credentials must come from `ANTHROPIC_API_KEY` in `RunSpec.Env`. Managed
  (enterprise) settings still apply; they are the operator's responsibility.
  Claude Code may still auto-approve its own read-only shell commands (for
  example `date`) inside an allowed `Bash`.
- Empty `AllowedTools` means `Read Grep Glob` (read-only).
- Events come from `assistant` (text, `tool_use`) and `user` (`tool_result`)
  messages. The final `result` message gives the answer text, `num_turns`,
  `total_cost_usd` and `usage` (input, output, cache read, cache creation).
  `is_error` results (for example `error_max_turns`) fail the run.
- Credentials: `ANTHROPIC_API_KEY` via env (the machine's own login is not used).

## codex

Written against the Codex CLI docs for `codex exec` (non-interactive mode,
`--json` JSONL events). **Not run against a real binary** (not installed on
the development machine); the recorded streams in `internal/runtime/testdata`
follow the documented event shapes. Verify on a machine with Codex before
Gate 0.

```
codex exec --json --skip-git-repo-check --ephemeral -c approval_policy="never" --sandbox <read-only|workspace-write> \
  [--cd <workdir>] [--model <model>] -
```

- Codex has no tool allow list or turn limit: `AllowedTools` only picks the
  sandbox (`workspace-write` when it holds `Edit`, `Write`, `MultiEdit` or
  `NotebookEdit`, otherwise `read-only`). `MaxTurns` is ignored.
- Events: `thread.started`, `turn.started`, `turn.completed` (usage),
  `turn.failed`, `error`, and `item.*` with item types `agent_message`,
  `command_execution`, `file_change`, `mcp_tool_call`, `web_search`
  (`reasoning` and `todo_list` are skipped). A `command_execution` or MCP call
  whose status is `failed` or `declined` is a tool error. `turn.completed` usage is a running total, so the last one
  is used. Input tokens exclude
  the cached part, which is reported as cache read; `cache_write_input_tokens`
  is cache write. The approval policy is set to `never` explicitly. Codex reports no cost, so
  `CostUSD` is 0.
- Credentials: `CODEX_API_KEY` via env.
- Each run sets `CODEX_HOME` to a fresh empty temporary directory, removed
  afterwards, so the worker account's `~/.codex` (config, MCP servers, login)
  cannot change what the job was given. The machine's own login is not used:
  credentials must be `CODEX_API_KEY`, in `RunSpec.Env` or the worker's
  environment, which `codex exec` reads directly. Without it the run fails at
  once with an error naming the variable.
- With a job MCP gateway (below) it gets `-c` overrides for a streamable
  HTTP server `mcp_servers.forecrew`: `url`, `bearer_token_env_var =
  "FORECREW_JOB_MCP_TOKEN"`, `enabled_tools` (the allowed gateway tools),
  `default_tools_approval_mode = "approve"` (so `approval_policy = "never"`
  does not refuse them) and `tool_timeout_sec = 2100`.

## opencode

Serves every model the other two do not (plan decision 33). Written against
**opencode 1.19.5** (`opencode-ai` on npm, `v1.19.5` at
`github.com/anomalyco/opencode`): its docs and the source of that tag.
**Not run against a real binary** (not installed on the development machine):
see the live checks in [runbooks/smoke-live.md](runbooks/smoke-live.md).

```
opencode run --format json --model <provider/model> --dir <workdir> [--session <id>]
```

- **Exact version.** `Run` refuses any other version than
  `runtime.OpencodeVersion`, because what keeps a repo out was read in that
  release's source, the JSON events are undocumented, and 2.x renames the
  `bash` permission. A new version means reading that again and running the
  live checks.
- **Isolation.** Each run gets a fresh temporary directory, removed afterwards:
  `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, `XDG_STATE_HOME` and
  `OPENCODE_TEST_HOME` (opencode's home, for `~/.opencode` and `~/.claude`)
  point inside it, so the account's config, login and sessions are not used.
  `HOME` stays for the agent's shell, whose toolchains live there; the XDG
  variables reach the shell too, so caches that follow them start cold. The
  worker's other `OPENCODE_*` variables are removed, except `OPENCODE_API_KEY`.
  A managed config (`/etc/opencode`) still applies; it is the operator's.
- **Configuration** is inline, in `OPENCODE_CONFIG_CONTENT`: no file, and
  no secret, which it names as `{env:VAR}`. It turns off auto-update, sharing,
  snapshots, formatters and language servers (both run binaries of the
  checkout, even for a role without a shell), and enables the model's provider
  only. `OPENCODE_DISABLE_MODELS_FETCH` keeps the catalog built into the binary,
  and the model is declared, so a model newer than that catalog still resolves
  (without prices: its cost is 0). `OPENCODE_PURE` drops external plugins; the built-in
  ones stay, because Azure, Cerebras and others need them. opencode still tries
  to install `@opencode-ai/plugin` from npm into the empty config directory, in
  the background; nothing of the job depends on it.
- **The checkout widens nothing.** Inline config only wins key by key over
  `opencode.json` and `.opencode/`, so a `bash` rule of the repo would survive
  the merge, and `.opencode/` also brings plugins, tools and agents.
  `OPENCODE_DISABLE_PROJECT_CONFIG=true` skips all of them and project
  instruction files (`AGENTS.md`, `CLAUDE.md`): the role's prompt is what the
  agent gets. `OPENCODE_DISABLE_CLAUDE_CODE` and
  `OPENCODE_DISABLE_EXTERNAL_SKILLS` do the same for `.claude` and `.agents`.
  A `.env` in the checkout is not read: the release binary is built without it.
- **Directory.** opencode takes its directory from `PWD`, which Go leaves as the
  worker's own, so the run sets `--dir` and `PWD`.
- **Prompt** on stdin. `--auto` is never passed.
- **Limits.** `MaxTurns` is the `steps` of the `build` agent: after that many
  steps opencode takes the tools away and asks for a final answer, so the job
  ends with what it has instead of failing as on Claude Code.
- **Structured output.** `opencode run` cannot constrain output to a schema. The
  schema is in the prompt (the context pack puts it there), `output.go`
  validates the last message, and an `InvalidOutputError` is sent back once, with
  the validator's message and the schema, with `--session` in the same run
  directory. It gets what is left of the timeout and the turns. A second failure
  returns `ErrInvalidOutput`.
- **Events** (undocumented; from `run.ts`): `step_start` (a turn), `step_finish`
  (that step's tokens and cost, summed), `text`, `tool_use` (once, when the call
  has ended; status `completed` or `error`) and `error`. Input tokens exclude the
  cache; output tokens include reasoning. The first `sessionID` is the session.
  Gateway tools are reported as `mcp__forecrew__<tool>`.
- **Credentials** come from the provider's own variable in `RunSpec.Env` or the
  worker's environment. With `RunSpec.Endpoint` the model's provider becomes
  `@ai-sdk/openai-compatible` with that `baseURL` and `apiKey: {env:<key_env>}`;
  the key must be set, and one from the worker's environment is redacted too.

### Permissions

opencode allows nearly everything by default, so the rules start from
`"*": "deny"` and allow only what `AllowedTools` maps to (`OpencodeRules`). Its
last matching rule wins, and these come after opencode's defaults. No rule is
`ask`: a tool that is not allowed is removed or refused, never waits.

| `AllowedTools` | opencode |
|----------------|----------|
| `Read` | `read`, `list` |
| `LS` | `list` |
| `Glob` | `glob` |
| `Grep` | `grep` |
| `Edit`, `Write`, `MultiEdit`, `NotebookEdit` | `edit` |
| `Bash` | `bash: allow` |
| `Bash(git log *)` | `bash: {"*": deny, "git log *": allow}` |
| `Bash(go test:*)` | `bash: {"*": deny, "go test *": allow}` |
| `TodoWrite` | `todowrite` |
| `WebFetch` | `webfetch` |
| `WebSearch` | `websearch` |
| `mcp__forecrew__<tool>` | `forecrew_<tool>` |
| anything else | the job fails at start |

Empty `AllowedTools` means `Read Grep Glob`. `external_directory` is denied
except `<EvidenceDir>/*`, which also overrides the temporary directories
opencode allows by default. `doom_loop` is denied. opencode checks each command
of a shell line on its own, so `git log; curl x` does not pass `git log *`.

## Custom endpoints

`RunSpec.Endpoint` comes from `runtimes.<name>.base_url` (config.md), which
`config` validates, because repo code and keys go there. `claude-code` sets
`ANTHROPIC_BASE_URL`, `codex` passes `-c openai_base_url=...` and `opencode`
configures a provider. The key is never in the URL, argv or a file.

## Job MCP gateway

When the job spec has `job_mcp` (docs/contracts/job-mcp.md), the worker
sets `RunSpec.JobMCP` to `<server_url>/job-mcp`, built from its own configured
forecrewd URL (`job_mcp.url` in the spec is informational and not used), and
puts the token in `RunSpec.Env` as `FORECREW_JOB_MCP_TOKEN`. The token is never in argv or a
file: each runtime's MCP configuration refers to the variable, and the
variable's value is redacted like every other secret. The agent's own
processes inherit the variable; the token only reaches the gateway tools of
this job, and only while its lease lasts.

The role's job MCP tools (`ask_human`, `evidence.attach`, ...) are in the
job's allowed tools as `mcp__forecrew__<tool>` (the gateway names its
server `forecrew`).

- **claude-code:** `--mcp-config` with one `http` server `forecrew`, its
  `Authorization` header `Bearer ${FORECREW_JOB_MCP_TOKEN}` (Claude Code
  expands the variable when it connects; the name is not one of the
  credential variables it reads as empty) and a `timeout` of 35 minutes,
  above `ask_human`'s 30. `--strict-mcp-config` ignores every other MCP
  configuration. The `mcp__forecrew__*` tools are pre-approved with
  `--allowedTools` and left out of `--tools`, which lists built-in tools only.
- **codex:** the `-c mcp_servers.forecrew.*` overrides above.
- **opencode:** a `remote` server `forecrew` in the inline configuration, its
  `Authorization` header `Bearer {env:FORECREW_JOB_MCP_TOKEN}`, OAuth off and a
  `timeout` of 36 minutes. opencode names the tools `forecrew_<tool>`; only the
  allowed ones have an allow rule, so the others are removed.

Not yet verified against the real CLIs: see the live checks in
[runbooks/smoke-live.md](runbooks/smoke-live.md).

## Toolbox containers

With a toolbox (docs/worker.md, Toolboxes) the worker sets `RunSpec.Container`
and `RunSpec.MCPServers`. Every adapter then runs its CLI as the container's
`docker run` (`Container.Wrap`), with the adapter's own binary name, not the
operator's path, the same work and evidence directories, and exactly
`RunSpec.Env` plus the adapter's own variables as the environment: nothing
of the worker's. Each run's config home is the container's empty in-memory
`HOME` (`CLAUDE_CONFIG_DIR` or `CODEX_HOME` = `/home/agent`) instead of a
temporary directory.

The toolbox's stdio MCP servers (`RunSpec.MCPServers`) are added next to the
gateway:

- **claude-code:** `--mcp-config` gets a `stdio` entry per server
  (`command`, `args`, `env`); a name in `credential_env` becomes
  `"NAME": "${NAME}"`, which Claude Code expands from its own environment,
  so no value is in argv. Without a gateway the config still holds the servers;
  `--strict-mcp-config` is always set. Allow a server's tools with
  `mcp__<server>` in the allowed tools.
- **codex:** `-c mcp_servers.<name>.command`, `.args`, `.env` (inline table),
  `.env_vars` (the `credential_env` names, which codex forwards from its
  environment), `default_tools_approval_mode="approve"` and
  `startup_timeout_sec=60`.

Not yet verified against the real CLIs with a model: see the live checks in
[runbooks/smoke-live.md](runbooks/smoke-live.md). The browser toolbox's smoke
test (`make test-toolbox-browser`) runs `claude mcp list` in the container,
which starts the server and reports it connected.

## Evidence scratch directory

`RunSpec.EvidenceDir` is a per-job directory outside the checkout, also in
`FORECREW_EVIDENCE_DIR`. claude-code gets it with `--add-dir`; codex gets
`--add-dir` too, which makes it writable only in the `workspace-write` sandbox;
opencode gets an `external_directory` allow rule.

Limitations: codex keeps the `read-only` sandbox for every role without an
editing tool, and that sandbox lets it write nowhere. So no read-only role on
codex can create scratch files, whatever other tools it has; a role that needs
evidence should run on claude-code. On any runtime, a read-only role with
neither `Write` nor `Bash` has no way to create a file either.

## scripted

`NewScripted(path, cfg)` replays a JSON script (`runtime.Script`) on every
run: events to emit (with optional `delay`), file edits in the work dir (paths
must stay inside it), `commands` (programs run in the work dir inside the
toolbox container, after the events and before the edits: their output goes to
the job log as `log` events and a non-zero exit code is logged, not an error;
without a container the run fails), the final `text` and `output`, usage, and an optional
`error`. It validates the output like the real adapters.

```json
{
  "events": [{"type": "message", "text": "working"}],
  "edits": [{"path": "src/a.go", "content": "package a\n"}],
  "text": "Done.",
  "output": {"summary": "added a.go"},
  "turns": 1,
  "usage": {"input_tokens": 10, "output_tokens": 5},
  "cost_usd": 0.01
}
```

## Tests

Adapter tests run the test binary itself as the fake CLI (helper-process
pattern, `FORECREW_FAKE_CLI=1`), replaying the recorded streams in
`internal/runtime/testdata`. No network and no real CLI are needed.
