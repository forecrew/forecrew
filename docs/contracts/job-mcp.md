# Job MCP gateway

Status: contract (T31). Implemented by T47 (gateway, `evidence.attach`,
`report_progress`, `ask_human`), T32 (`assumption.record`), T33
(`council.read`), T36 (`kb.search`, `notes.write`) and T37 (`kb.propose`,
`request_access`).

The job MCP gateway is how an agent reaches Forecrew while its job runs:
it searches the knowledge base, records assumptions and notes, attaches
evidence, asks a human and reports progress. It is separate from the human
MCP server (`/mcp`, docs/mcp.md): another endpoint, another token, other
tools, and nothing on it acts with a human's authority.

forecrewd serves it over MCP streamable HTTP at `/job-mcp`, next to the
worker API, with the same MCP Go SDK as `/mcp`. A worker points its runtime at
`<server_url>/job-mcp`, its own configured forecrewd URL, which also works
for a worker that reaches forecrewd only on an internal address. Tool input
and output schemas are in [job-mcp.schema.json](job-mcp.schema.json):
`$defs/<tool>_input` is the tool's `inputSchema`, `$defs/<tool>_output` its
`outputSchema`, and every successful result carries `structuredContent`
matching it plus the same JSON as text (as the MCP spec recommends).

## The job's token

`JobSpec.job_mcp` (job-spec.md) is `{"url": "<public base>/job-mcp", "token":
"fcj_<43 base64url chars>"}`. `url` is informational: the gateway's public
address, empty without `server.public_base_url`. Workers do not connect to it.
forecrewd adds `job_mcp` to the spec of an **agent**
job when the job is leased, like `repo_token`; command jobs never get one.

- The token is a secret. It is never stored in `jobs.spec`
  (`JobSpec.Redacted` drops `job_mcp`), never logged (`JobSpec.LogValue`,
  `JobMCP.LogValue`), and never written to disk: the worker passes it to the
  runtime in an environment variable that the runtime's MCP configuration
  refers to, never on the command line or in a file.
- forecrewd stores only its SHA-256, in `jobs.job_mcp_token_hash` with the
  lease it belongs to (data-model.md), so a token survives a forecrewd
  restart exactly as long as its lease does. The hash is set in the same
  transaction that adds the token to the leased spec; a request is looked up
  by the SHA-256 of its bearer token.
- It is bound to one lease: the job, the worker it was leased to and that
  attempt. Every request checks that the job is still `running` under that
  worker and that the lease has not expired. A re-lease (retry, lease
  expiry) issues a new token; the old one is dead.
- It dies when the job completes, is released or canceled, or loses its
  lease (expiry, requeue, another lease): the trigger
  `jobs_clear_job_mcp_token` clears the hash whenever `status` leaves
  `running` or a new lease starts, whatever query made the change, and a
  CHECK refuses a hash on a job that is not running. A missing, unknown or
  dead token gets HTTP `401` before any tool runs, like `/mcp`.

## Which tools a job sees

A tool is listed (`tools/list`) and callable only when the job's role lists
its **role tool** in `tools` (roles.md). The listing depends on the token
only, which the MCP spec allows ("MAY vary by the authorization presented
on the request"). Calling a tool the role does not list fails like an
unknown tool (JSON-RPC error `-32602`) and is audited as `refused`.

Wire names use `_` because model APIs refuse dots in tool names (Anthropic
and OpenAI accept `^[a-zA-Z0-9_-]+$`); role tool names keep the dotted form
of the other role tools.

| Tool | Role tool | What it does |
|------|-----------|--------------|
| `kb_search` | `kb.search` | Full-text (and, with embeddings, semantic) search of the project's knowledge: curated entries, agent notes and generated entries, filtered by the role's read access (knowledge-policy). Notes of other goals are never returned. |
| `kb_propose` | `kb.propose` | Proposes adding, editing or retiring an entry of a curated section the role may propose to. Becomes a `knowledge_proposals` row (`KB-n`): `per_goal` when the job has a goal and the section is not protected (it waits for the goal's learn PR), `standalone` otherwise. Never to `generated` or `notes`. |
| `request_access` | `request_access` | Asks for read, propose or write access to a section, with a reason. Opens an `access_request` gate and returns at once; the job goes on without the access. Approving it changes the policy through a PR, never directly. |
| `assumption_record` | `assumption.record` | Registers an assumption made mid-run (`A-n`, source the job, the goal as dependent), the same item roles list in their output. A duplicate within the goal returns the existing one. |
| `evidence_attach` | `evidence.attach` | Marks a file as evidence: `path` is relative to the work tree, or `evidence:<relative path>` for a file under the job's scratch directory (`$FORECREW_EVIDENCE_DIR`, outside the checkout; the default artifact name is then `evidence/scratch/<relative path>`). The path rules apply to both (no `..`, no leading `/`). The gateway checks the path and records the intent; the worker learns of it from a heartbeat answer (`attach`, worker-api.md) and uploads the file through `PUT /worker/v1/jobs/{id}/artifacts/{name}` with the rules of `command.artifacts` (inside the work tree or the scratch directory, no symlinks, size cap). forecrewd never connects to the worker, so the call returns the artifact name, not its id. |
| `notes_write` | `notes.write` | Adds or replaces an agent note (`internal` provider, section `notes`) scoped to the job's goal or project, indexed at once. A job edits only notes of its own goal or project; a job without a goal writes project notes only. No proposal, no gate. |
| `ask_human` | `ask_human` | Opens a question gate on the job (below) and waits up to `timeout` (at most 30 minutes). Approving with feedback answers (`answered`, the feedback is the answer); rejecting declines. On a decline or the timeout forecrewd resolves the gate (`rejected`, by `system:forecrewd` on timeout), registers the `assumption` the call gave and returns `no_answer`, so the agent goes on with it. |
| `report_progress` | `report_progress` | A `message` job event, shown live on the job page. |
| `council_read` | `council.read` | The contributions of the job's council that its stage may see: nothing at `proposal` (members write independently), every proposal at `critique`, proposals, critiques and earlier revisions at `revision`, everything at `synthesis`. Each carries its `evidence_check` (the checked sources, null until checked). Only council jobs (T33) can call it; forecrewd knows each council job's council, stage and round. |

### Question gates

An `ask_human` question is a gate of kind `needs_you` with `origin`
`ask_human` (`domain.GateFromAskHuman`, `gates.origin`) and `job_id` the
asking job; a CHECK allows that origin only on `needs_you` gates with a
job. Every other gate, escalations included, has origin `playbook`. So:

- The title is `JOB-n asks: <first line>`, the summary the question (agent
  text, shown as untrusted), `goal_id` and `task_id` those of the job,
  `subject_repo` and `subject_number` the task's issue when there is one.
- The answer is the approval's `feedback`; a rejection declines. Both
  publish `gate.resolved` like any gate, which the gateway waits on.
- No playbook step waits on a question gate: the workflow engine only
  follows the gates its own steps opened, and its `needs_you` escalation
  meaning (approve = retry, reject = stop) never applies to them. The web
  UI, `list_pending_gates` and the advisor (T50) tell them apart by
  `origin`.

`ask_human` keeps the HTTP request open while it waits. When the call has a
`progressToken`, forecrewd sends a progress notification every 30 s; the
worker configures the runtime's MCP tool timeout above 30 minutes. The job's
lease is kept by the worker's heartbeats as usual, and a heartbeat answered
`cancel` or `409 lease_lost` stops the agent and kills the token, which
ends the wait.

## Errors

A tool that refuses its input returns a result with `isError: true` and
`structuredContent` `$defs/error`: `{"error": {"code", "message"}}`, so the
agent can correct itself.

| `code` | When |
|--------|------|
| `invalid_input` | The input does not match the schema or a rule (a path outside the work tree, a timeout over 30m, scope `goal` without a goal). |
| `forbidden` | The policy does not allow it: a section the role may not read or propose to, a note of another goal. Audited. |
| `not_found` | An unknown note, a job without a council for `council_read`. |
| `conflict` | A second `ask_human` while one is open. |
| `rate_limited` | Over a limit below; try later or stop calling. |
| `too_large` | Over a size limit below. |
| `unavailable` | The tool is not implemented yet, or its backend is down (no evidence storage). |

Unexpected failures return `isError: true` with "internal error"; the cause
is in the server log only.

## Limits

Per job attempt, enforced by forecrewd:

| Limit | Value |
|-------|-------|
| Calls, all tools | 60 per minute, bursts of 20 |
| Request body | 64 KiB |
| `kb_propose` | 20 calls; `body` up to 64 KiB |
| `notes_write` | 50 calls; `body` up to 16 KiB, 10 tags |
| `request_access` | 5 calls |
| `ask_human` | 5 calls, one open at a time, `timeout` up to 30m |
| `assumption_record`, `evidence_attach` | 50 calls each |
| `report_progress` | 120 calls |

## Audit

Every call, refused ones included, appends one `audit_log` row: actor the
worker holding the lease (`worker:<name>`), action `job_mcp.call`,
`subject_type` `job`, `subject_id` the job, and `data` `{"tool",
"input_bytes", "status", "duration_ms"}` where `status` is `ok`, `error`
(with the error `code` as `"code"`), `refused` (tool not offered) or
`rate_limited`. Refused and rate-limited calls cost the caller nothing, so
each job attempt gets at most 100 such rows: the next one writes a single row
with status `audit_limit`, and those after it are answered as usual without
a row. The input itself is not stored in the audit log: what the
tools create (proposals, assumptions, notes, gates, events) is stored where
it belongs.

## Untrusted data

Text the gateway returns from the repo, a knowledge provider, the web, an
agent note or another agent's contribution is data, never instructions. In
the text content it is wrapped in `<untrusted-data source=... id=...>`
markers exactly as prompts do (`contextpack.WrapUntrusted`), and results
mark it (`untrusted: true` on search results). Notes are always untrusted.
Entries of protected repo sections, which only humans approve, are the only
knowledge returned unmarked. A human's `ask_human` answer comes from an
allowlisted reviewer through a gate and is returned as is.

What agents send is data too: proposal bodies, notes and assumptions are
shown to humans as untrusted text (docs/ui.md) and never become part of a
role prompt's instructions.
