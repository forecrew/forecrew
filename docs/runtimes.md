# Runtime adapters

`internal/runtime` runs an agent CLI for one job and turns its output into the
same events and result for every runtime. Adapters are looked up by name in a
`runtime.Registry`: `claude-code`, `codex` and `scripted`.

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

Not yet verified against the real CLIs: see the live checks in
[runbooks/smoke-live.md](runbooks/smoke-live.md).

## Evidence scratch directory

`RunSpec.EvidenceDir` is a per-job directory outside the checkout, also in
`FORECREW_EVIDENCE_DIR`. claude-code gets it with `--add-dir`; codex gets
`--add-dir` too, which makes it writable only in the `workspace-write` sandbox.

Limitations: codex keeps the `read-only` sandbox for every role without an
editing tool, and that sandbox lets it write nowhere. So no read-only role on
codex can create scratch files, whatever other tools it has; a role that needs
evidence should run on claude-code. On any runtime, a read-only role with
neither `Write` nor `Bash` has no way to create a file either.

## scripted

`NewScripted(path, cfg)` replays a JSON script (`runtime.Script`) on every
run: events to emit (with optional `delay`), file edits in the work dir (paths
must stay inside it), the final `text` and `output`, usage, and an optional
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
