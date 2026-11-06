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
| `connection` | connection | `connections` | `connection.manage`, No escalation |
| `domain` | email domain | `verified_domains` | `connection.manage` |
| `member` | user ID, or an email for an invitation | `memberships`, `invitations` | `member.manage`, No escalation |
| `team` | slug | `teams`, `team_members` | `team.manage`, No escalation |
| `role_binding` | `<kind>:<subject>/<role>[@<project>]` | `role_bindings` | by subject and scope, No escalation |
| `provider` | location | `capacity_providers` | `worker.manage`; `billing.manage` for its [budget fields](permissions.md#budget-fields) |
| `shape` | machine type | `shapes` | as `provider` |
| `fleet_policy` | `fleet` | `fleet_policy` | as `provider` |
| `schedule` | schedule | `config_settings` of the organization | `worker.manage`. Stored only: nothing acts on it before the worker schedules of 4.4 |
| `integration` | project | `.forecrew/profile.yaml` | `project.manage` on the project |
| `knowledge_policy` | project | `.forecrew/knowledge-policy.yaml` | `project.manage` on the project |
| `repo_file` | `<project>:.forecrew/<path>` | the file | `project.manage` on the project |

Each area has a read permission: `org.read`, `worker.read` or
`connection.manage`; `billing.manage` also reads `provider`, `shape` and
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

**Resources without a delete.** Deleting a connection or a shape, or reverting
its creation, sets `enabled: false`. Deleting a provider revokes it. A server
setting's delete drops the stored value and the file's counts again.

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
instance connection. A revert with one of those effects counts too, because it is a
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
  the reference. There is no MCP tool for it.
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
  untrusted data. A plan that draws on such text says so in `untrusted`.
