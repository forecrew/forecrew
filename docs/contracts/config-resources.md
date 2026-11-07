# Configuration resources

Every setting of Forecrew is a declarative resource, and every change is a
plan a person confirms (plan decision 31). Forms, YAML, the CLI, MCP clients
and the assistant write the same resources through the same plan, permissions
and history. The code is `internal/config/resource`; the secret store is
`internal/secrets`; the tables are in [data-model.md](data-model.md).

## Resources

A resource is `kind`, `name` and `spec`. Each area registers `Schema`, `Read`,
`Validate`, `Diff` and `Apply` on top of the existing services, so a resource
keeps the rules of its service: teams and role bindings go through the tools
that check No escalation. A test fails when a field of a setting has no place
in a spec and no reason to be left out, or a key of `forecrew.yaml` is neither
a resource nor listed as a bootstrap setting.

| Kind | Name | Backed by | Changing it needs |
|------|------|-----------|-------------------|
| `organization` | `organization` | `accounts` | `org.manage` |
| `runtime` | runtime | `config_settings` over `runtimes` | `org.manage` |
| `pool` | pool | `config_settings` over `pools` | `pool.manage` |
| `auth` | `auth` | `config_settings` over `auth` | `org.manage` |
| `model_connection` | connection | `model_connections` | `models.manage` |
| `model_assignment` | `organization`, or `project:<project>` | `model_assignments` | `models.manage`; `models.assign` on the project for `project:` |
| `connection` | connection | `connections` | `connection.manage`, No escalation |
| `domain` | email domain | `verified_domains` | `connection.manage` |
| `member` | user ID, or an email for an invitation | `memberships`, `invitations` | `member.manage`, No escalation |
| `team` | slug | `teams`, `team_members` | `team.manage`, No escalation |
| `role_binding` | `<kind>:<subject>/<role>[@<project>]` | `role_bindings` | by subject and scope, No escalation |
| `provider` | location | `capacity_providers` | `worker.manage`; `billing.manage` for its [budget fields](permissions.md#budget-fields) |
| `shape` | machine type | `shapes` | as `provider` |
| `fleet_policy` | `fleet` | `fleet_policy` | as `provider` |
| `schedule` | `<playbook>:<project>` | `config_settings` over `schedules` | `org.manage`; the playbook must be one forecrewd can schedule |
| `integration` | project | `.forecrew/profile.yaml` | `project.manage` on the project |
| `knowledge_policy` | project | `.forecrew/knowledge-policy.yaml` | `project.manage` on the project |
| `repo_file` | `<project>:.forecrew/<path>` | the file | `project.manage` on the project |
| `tracker_connection` | connection | `tracker_connections` | `trackers.manage` |
| `tracker_route` | `<connection>/<position>` | `tracker_routes` | `trackers.manage` |

Each area has a read permission: `org.read`, `worker.read`, `models.read`,
`connection.manage` or `trackers.read`; `billing.manage` also reads `provider`, `shape` and
`fleet_policy`, whole, to change their budget fields. What a caller cannot read is left out of `config_get`
and `config_export`, the assistant included, and a plan touching it is
refused.

**Scope.** `config_settings.account_id` NULL is the scope of the whole server.
Server settings are changed through a plan only on a server with one
organization; on a shared server they stay in the file until they move to
organization scope (T69). A stored `runtime` names only `credential_env`
variables a runtime of the file uses, and a stored instance connection only
the `client_secret_ref`s of the file or `store:` references, checked when a plan
is made and at startup: forecrewd sends these values to `base_url` and to the
issuer, and its environment also holds its own secrets. `origin` says whether a value comes from the `file`
or the `database`. A stored server setting counts from the next start of
forecrewd, and its plan says so.

**Repo files** apply by opening a pull request on the branch
`forecrew/config-<plan>`, and only under `.forecrew/`. A plan touches one repo
and holds nothing else, because a pull request and a transaction cannot be
one step.

**Trackers.** A `tracker_connection` spec is `kind`, `config` and `triggers`,
as `create_tracker_connection` takes them; a config key ending in `_ref` names a
value of the secret store (`store:<name>`), which the plan asks for. What the
adapter reads from the tracker itself, such as Jira's `deployment`, is no
setting: it is left out of the spec, kept while the rest of the config stays,
and read again by the connection test. A plan does not reach the tracker, whose
token it has not put in place yet: a new or changed connection gets a `connect`
step, `forecrew tracker test <name>`. A route's states are checked against the
tracker when its connection exists before the plan. Routes are named by their
connection and position (`jira/1`); a route's `warning` is the tracker's to
set and no setting.

**Resources without a delete.** Deleting a connection or a shape, or reverting
its creation, sets `enabled: false`. Deleting a provider revokes it. A server
setting's delete drops the stored value and the file's counts again.

## Model connections

Plan decisions 33 and 34. An organization connects its providers once, and
every job of the organization gets its own organization's key and model. The
code is `internal/models`; nothing else decides a job's connection.

- **`model_connection`** is a runtime owned by an organization:
  `{vendor, adapter, base_url, key_env, secret_ref, enabled}`. `adapter` is
  `claude-code`, `codex` or `opencode`; `base_url` follows the rules of
  `runtimes.<name>.base_url` (config.md). The key goes in the variable of the
  adapter (`ANTHROPIC_API_KEY`, `CODEX_API_KEY`); `opencode` names its own in
  `key_env`, uppercase and ending in `_KEY`, not `FORECREW_*`. `secret_ref` is
  `store:models/<name>`: the namespace `models/` of the secret store is for
  model keys only, and no other resource may name it, so whoever manages a
  connection cannot send a sign-in secret to their endpoint, nor the other
  way round. `enabled` defaults to true. Deleting a connection deletes the
  row; its key stays in the store for a revert. A key is changed by naming a
  new secret, as everywhere.
- **`model_assignment`** says which model serves what: `tiers` (tier →
  vendor → model), `roles` (role → `{connection, tier}`) and, on
  `organization` only, `project_overrides` (the policy
  `models.project_overrides`, default true). `project:<project>` holds a
  project's overrides. They live in the database and never in `.forecrew/**`:
  the model decides cost and where code goes, so a pull request or an agent
  must not change it.
- **Resolution**, for a role in a project: the project's override (unless the
  policy is off), then the organization's assignment, then `roles` of
  forecrew.yaml. Tiers are looked up the same way, per vendor. An override or
  assignment that cannot be used never falls through to the next level: the
  job waits. A council seat on another agent takes the first enabled
  connection of that agent, or the server's runtime; a role that a project
  override keeps on one connection keeps its seats there.
- **The server file.** `runtimes`, `model_tiers` and `roles` stay the bootstrap
  default of a self-hosted server and read with origin `file` ("Managed by
  server file"); an assignment may name a runtime of the file. With
  `models.server_keys_for_organizations: false` (hosted) the file is not
  shown, cannot be named, and a job with no connection of its organization
  does not start.
- **At lease** forecrewd reads the key from the store, in the job's
  organization, and sends key, endpoint and model from one decision. Nothing
  of it is stored with the job; a rotated key applies from the next lease with
  no change on workers. The pool rule (`credentials.runtimes`) applies to
  connection names; an organization's connection whose pool withholds its key
  waits rather than run with a key of the worker. A job on an organization's
  connection carries `organization_model: true`, and its endpoint
  `organization: true` (job-spec.md): the worker gives the agent only an
  allowlist of its own variables (job-spec.md), so no key, token or secret of
  its own and nothing that sends the key or code elsewhere. Such a job needs the label
  `models:organization`, which forecrewd gives only to a worker that polls
  with the capability `organization_models` (worker-api.md) and to machine
  shapes; an older worker never leases it, and the job waits for another.
- **Waiting.** A queued job with no usable connection gets runtime `none`, so
  no worker leases it and no machine starts for it, and `job_status` gives the
  reason `no_connection` with a sentence naming the role and what to do. When
  a connection is there, the job moves to its runtime and model. forecrewd
  settles queued jobs before each lease and each fleet pass.
- **Plans** check the model changes together: every assigned role must end up
  on a connection that exists, is on and has a model for its tier, a role
  with a toolbox is not put on `opencode`, and the reviewer stays off the
  developer's vendor unless `policy.allow_same_vendor_review`. So disabling or
  deleting a connection a role uses is refused unless the same plan reassigns
  the role, and the plan's `affects` lists the roles and projects on it. The
  check runs again in the transaction that applies the plan.
- **Audit:** `model_connection.created`, `.rotated`, `.disabled`, `.enabled`,
  `.changed`, `.deleted` and `model_assignment.changed`, `.deleted`, with
  references, never a value. Export shows references.

## Plans

A plan (`config_plans`) is a list of changes with a diff of each field, the
permissions needed, what it affects, a cost hint, and whether it can be
reverted. It is valid for 24 hours; past that it reads as `expired`.

- `pending` -> `awaiting_approval` -> `applied` -> `reverted`, or `canceled`
  while open. An hourly sweep stores `expired` and drops what the plan staged.
- **Validation** refuses the whole plan with every problem. A spec with a key
  the schema does not have is refused: nobody can propose a setting that does
  not exist.
- **Applying** happens in one transaction: every change, the status and the
  audit entry, or nothing. The plan stores the digest of what it changes; when
  those resources moved since, it is refused and has to be rebuilt.
- **Revert** makes a new plan with the old values, confirmed like any plan.
  Applying it marks the first plan `reverted`. What cannot come back is stated
  in `undo` before the first confirm.
- **Audit:** `config.plan.created`, `.approval_requested`, `.canceled`,
  `.applied`, `.denied` and `secret.saved`, with the name only, as the person.

### Confirmation

Confirming is a step of the protocol, not a parameter.

| Plan from | Confirmed by |
|-----------|--------------|
| the assistant, an MCP client (`source` `assistant`, `mcp`) | a person in the web UI: `apply` with the session cookie. The API sets this from the request; no field of the body or tool input can |
| forms, the CLI, YAML (`form`, `cli`, `yaml`) | a person, by the web UI or by a personal token; `forecrew config apply` shows the plan and asks |
| any source, when the plan lowers protection | a person in the web UI, with the second confirmation |

A plan from an ownerless token has source `mcp` whichever entry point it came
through: no person stands behind it. A session means a real browser session; a
token in the cookie of `token_login` does not count.

Service accounts, jobs and workers never confirm. `config_apply` over MCP never
applies: it moves the plan to the Inbox and answers with the plan and a link
for the client to show. Of the options of the MCP specification (2025-11-25)
this is the safest: a form elicitation is answered through the client, which
may be the agent itself, and URL elicitation needs a session this stateless
server does not hold, while the link gives the same out-of-band step. The link
is not pre-authenticated; the person signs in as themselves.

Someone without the permissions asks for approval instead
(`request-approval`). `config_plans` with `inbox` lists the plans waiting for
a caller who could confirm them: those awaiting approval and those from MCP
clients.

### Lowering protection

A plan that lowers protection lists why in `lowers_protection`, and `apply`
needs a browser session and `word`, the organization's slug, typed by the same
person: adding or removing an Owner, turning off or deleting a sign-in
connection, turning off sign-in enforcement, opening network access, adding Forecrew
Cloud as a fallback, moving a machine type to another trust zone, giving a pool
a repo write token, adding an instance admin, allowing token sign-in, removing
the token cap, changing a runtime's `base_url` or `credential_env`, changing an
instance connection, creating a model connection, changing its `base_url`,
`adapter`, `key_env` or `secret_ref`, or turning it on again, moving a role of
a `model_assignment` to another connection (adding a role, changing its
connection, taking it off, deleting a project's overrides). A revert with one of those effects counts too, because it is a
plan like any other.

### Permissions

`config.apply` lets a caller make plans. Applying takes it, what each change
needs (the table above) and, for the budget fields, `billing.manage`: a change
to a budget field needs it, a change to any other field `worker.manage`, and
neither covers the other. Permissions are checked again when applying, as of
that moment, on the person confirming. The tools under the areas then check
No escalation against the real rows.

## Secrets

The assistant never sees or asks for a secret.

- `store:name` in a spec refers to the secret store, in the organization of the
  resource. A plan with such a reference gets a step `secret_required`, and
  cannot be confirmed until its value is there.
- The value goes to `PUT /api/v1/config/plans/{plan}/secrets/{secret}` or
  `forecrew config secret`, and from there to the store, staged under
  `staged/<plan>/<name>`. It is write-only: no API returns it, and `export` shows
  the reference. There is no MCP tool for it. Names starting `staged/` are
  reserved: no `store:` reference names one and only staging writes there, so a
  plan never reaches what another plan staged.
- **Nothing live changes before confirmation.** Applying moves a staged value to
  its name in the plan's transaction. A name that is saved already is never
  replaced: a step naming one says `reuses`, its plan uses the value there and
  takes no input, and a name taken after the plan was made refuses the plan. To
  change a secret, name a new one; the old value stays for a revert.
- Canceling a plan drops what it staged, and so does the sweep of expired plans.
- A provider never gets a `secret_required` step. Its plan has a `connect` step:
  the machine gets its token over its own connection.
- `internal/secrets` encrypts values with AES-256-GCM under
  `FORECREW_SECRET_KEY`, binding each to its scope and name. `secrets.Store`
  is the interface a Vault implementation takes.
- Text that looks like a secret (`domain.SecretPatterns`) is refused in
  conversations, plan titles, requests, repo files and imports, and is not
  stored. A reply of the assistant that holds one is replaced.

### Rotating the key

Every row of `secrets` names the key that encrypted it (`key_id`, a hash that
says nothing of the key).

1. Set `FORECREW_SECRET_KEY` to the new key and `FORECREW_SECRET_KEY_PREVIOUS` to
   the old one, and restart forecrewd. New values take the new key at once, and
   old ones keep reading.
2. At startup forecrewd encrypts every old value again. The log says `secret
   store rotated`; a failure is logged, and the next start goes on from there.
3. Once a start logged no failure, remove `FORECREW_SECRET_KEY_PREVIOUS` and
   restart.

A value under a key the server no longer holds does not read, and sign-in
through its connection fails with an error naming it; nothing is overwritten.

## The assistant

Role `assistant`: strong tier, read-only, pool `control`, job tools
`config.read`, `config.plan`, `ask_human`. Each turn of a conversation is a job.

- A conversation is personal: another person's reads as not found. Plans are
  shared; the sentence that asked for one stays on the plan.
- The job reads and plans as the person, with the credential they came with,
  so a Viewer, who has no `config.apply`, gets answers without plans.
- Its jobs run in a project the person may read, because jobs need one: the
  first, or the one named. Without any the assistant is unavailable, and forms
  do the job.
- Its answer is only text. Plans shown with a conversation are the rows with
  its `conversation_id`, drawn by the server: the model cannot put a Confirm
  button or a secure field on a screen, and "yes" in the chat confirms nothing.
- The answer of a turn is recorded when the conversation is next read, so
  nothing has to watch the job.
- Earlier turns and everything `config_read` returns reach the model as
  untrusted data. The server writes a plan's `untrusted` line from the gateway's
  audit of the job: a turn that called `config_read` or `kb_search` successfully
  gets it whatever the model says. `based_on` can only add to it.
