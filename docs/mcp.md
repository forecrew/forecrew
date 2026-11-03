# MCP server

forecrewd serves Forecrew over MCP (streamable HTTP) at `/mcp` on its public
URL (`server.public_base_url`). Every request needs an MCP token in the
`Authorization` header:

```
Authorization: Bearer fcm_...
```

Tokens start with `fcm_`, belong to one account and are shown once when
created. Only their SHA-256 is stored, so a lost token is revoked and
replaced. A missing, unknown or revoked token gets `401`. The token's name is
recorded as the actor (`mcp:<name>`) on everything it does.

## Connect from Claude Code

```sh
claude mcp add --transport http forecrew https://forecrew.example.com/mcp \
  --header "Authorization: Bearer $FORECREW_TOKEN"
```

Check with `claude mcp list`, or `/mcp` inside Claude Code.

## Connect from Claude apps

Custom connectors in the Claude apps (web, desktop and mobile) cannot send a
static `Authorization` header, so a bearer-token server like this one cannot
be added there directly. Use Claude Desktop with the `mcp-remote` bridge, which
adds the header. In `claude_desktop_config.json` (macOS:
`~/Library/Application Support/Claude/`, Windows: `%APPDATA%\Claude\`):

```json
{
  "mcpServers": {
    "forecrew": {
      "command": "npx",
      "args": [
        "mcp-remote",
        "https://forecrew.example.com/mcp",
        "--header",
        "Authorization:${FORECREW_AUTH}"
      ],
      "env": {
        "FORECREW_AUTH": "Bearer fcm_..."
      }
    }
  }
}
```

The space of `Bearer ` sits in `env` because Claude Desktop on Windows does
not escape spaces in `args`. Restart Claude Desktop after editing the file. It
needs Node.js for `npx`.

## Project config for Claude Code

To share the server with a team, put it in the project's `.mcp.json`; each
person sets `FORECREW_TOKEN` in their environment:

```json
{
  "mcpServers": {
    "forecrew": {
      "type": "http",
      "url": "https://forecrew.example.com/mcp",
      "headers": { "Authorization": "Bearer ${FORECREW_TOKEN}" }
    }
  }
}
```

## Tools

Tools take refs (`GOAL-3`, `JOB-41`, `GATE-7`) or UUIDs, and projects by name
or `owner/name`. Every result is a short text line plus structured content
(a JSON object; lists are wrapped, for example `{"workers": [...]}`) with the
GitHub links of issues, pull requests and gates.

| Tool | Purpose |
|------|---------|
| `onboard_project` | Register a repo (`repo`, optional `name`, optional `default_branch`, default `main`) and start onboarding; returns the project and the onboarding run |
| `create_goal` | Create a goal (`project`, `title`, `body`, optional `platforms` such as `web`, `ios`) and start its playbook |
| `job_status`, `job_logs` | Inspect a job (with why a queued job waits, e.g. `no online worker in pool control with labels runtime:codex`) and read its events |
| `list_workers` | Workers, their pool, labels, version, whether they are online and the jobs they run |
| `list_pending_gates` | Decisions waiting for a human: kind (`profile_approval`, `spec_approval`, `merge_approval`, `test_plan_approval` for the onboarding PR with the test strategy, matrix and environments, or `needs_you`), project, goal or task, summary, GitHub link, created time |
| `approve`, `request_changes`, `reject` | Resolve a gate. `request_changes` needs `feedback` and is not valid on `needs_you` gates (use `approve` to retry, `reject` to stop) |
| `dispatch_job` | Queue an ad-hoc job of a read-only role (`project`, `role`, optional `prompt` with the instructions, `needs`, `pool`, `goal`, `task`). `pool` defaults to the role's pool; a pool whose policy does not run the role is `invalid_input` (docs/worker.md, Pools) |
| `list_projects`, `get_project` | Projects |
| `list_goals`, `get_goal` | Goals; `get_goal` includes the latest spec, tasks (state, issue and PR links, check status, iterations, cost), the goal's gates with their decisions and the total cost |
| `list_jobs` | Recent jobs, filtered by `project`, `goal` and `status` |
| `cancel_job` | Cancel a queued or running job (`job`). A worker running it stops within one heartbeat (30 s) without pushing (if the heartbeat just before the push fails, the worker relies on the time-based lease check alone), and the playbook step waiting for it fails through its normal failure path (escalation, `finally`). A finished job is a conflict |
| `list_tokens` | The account's API and MCP tokens: `id`, `name`, `created_at`, `last_used_at`, `revoked_at`, and `current` on the one you are using. Never the secret |
| `revoke_token` | Revoke a token by `token` (name or ID); it stops working at once, as do the web sessions it backs. Revoking the token you are using is allowed: the answer has `self: true` and your next request gets `401`. Tokens are created only with `forecrew token create` on the server |

Invalid input and unknown refs come back as a tool error with a short message.
Unexpected failures return a generic "internal error"; the cause is in the
server log only.

## Security note

The `prompt` of `dispatch_job` is trusted: it goes into the job's prompt as
instructions, unlike text from GitHub users outside the allowlist, which is
marked untrusted. A manipulated MCP client (for example one that reads
untrusted content and follows it) therefore passes a trusted prompt on. Give
MCP tokens only to clients you trust, and revoke a token that may be
compromised.
