# Permissions and roles

What each person, team and service account may do (plan decision 26). The
catalog and the built-in roles are code in `internal/domain`
(`permission.go`: `domain.Permissions`, `domain.BuiltInRoles`,
`domain.GatePermission`, `domain.ScopesAllow`); this file describes them, and
a test keeps the seeded `roles` rows equal to `domain.BuiltInRoles`. The
model (organizations, members, teams, bindings, tokens) is in
[identity.md](identity.md). `internal/authz` enforces it; see [The
engine](#the-engine).

## Rules

- A **permission** is one action. A **role** is a set of permissions; a
  **role binding** gives a role to a user, a team or a service account on
  the organization or on one project.
- **Effective permissions** of a subject on a project are the union of the
  permissions of every binding that applies: its own bindings and, for a
  user, those of every team it is in; on the organization and on that
  project. There are no deny rules. Only active members, enabled users and
  enabled service accounts have any.
- Each permission is either **organization-wide** (only an organization
  binding grants it) or **project-scoped** (an organization binding grants
  it on every project, a project binding on that project).
- An **organization role** is bound on the organization only. A **project
  role** is bound on one project, or on the organization, where it applies
  to every project. Project roles hold project-scoped permissions only.
- **No escalation:** nobody gives a permission they lack, or acts on someone
  who holds more than they do; see [No escalation](#no-escalation).
- **Gates:** resolving a gate (`approve`, `request_changes` or `reject`,
  from any entry point, and `resolve_assumption` for the review gates it
  closes) needs `domain.GatePermission` of its kind on its project. Only
  people resolve gates: a person, or an ownerless token, which is a person's
  until it gets its owner. Service accounts, workers and agents never do,
  whatever role they hold. A `knowledge_proposal` gate is protected when one
  of the proposals linked to it through `gate_id` is in a protected section,
  by the policy of now; a gate without proposals, or one that cannot be
  placed or read, counts as protected. The learn gate of a goal ("Learnings
  of GOAL-n") also carries the goal's register files, `.forecrew/assumptions/A-n.yaml`
  and `docs/decisions/DEC-n.md`, which it names from the goal's assumptions
  and decisions as they are now: it is protected when a section whose globs
  hold one of them, whatever its file type and case, is protected, and a
  learn gate without proposals is protected only by its files. A section
  owned by `codeowners` that holds one leaves the merge to its reviewers.
- **Tickets:** resolving a `ticket_intake` gate needs `goal.create` on its
  project, because accepting a ticket creates a goal there; it is the one kind
  without a `gate.approve` permission of its own. Tracker comments never
  resolve a gate, and neither do `@forecrew approve` and `changes` on a GitHub
  issue that is a ticket. The one thing a tracker comment answers is an
  `ask_human` question, with `@forecrew reply`, as the mapped user and with
  `gate.approve.needs_you`. forecrewd rejects the `ticket_intake` gate of a
  ticket that was closed or accepted another way, as `forecrewd`.
- **Access requests:** an agent asks with `request_access`, which opens an
  `access_request` gate; only a person with `gate.approve.access_request`
  answers it, and what it grants still merges as a pull request.
- **The one exception on gates:** a PR merged or closed on GitHub resolves
  the `profile_approval`, `test_plan_approval`, `merge_approval` and `knowledge_proposal` gates
  whose subject it is, approved or rejected, as the actor `github:github`, and
  sends back the pending assumption reviews of its task as `forecrewd`. No
  permission is checked: the decision was made on GitHub, under GitHub's own
  rights over the repository, and Forecrew only records it. Whoever can merge
  in the repo can therefore pass these gates; protect the branch there.
- **Lists** without a project filter return only the rows of the projects
  where the caller has the list's permission.

## No escalation

Each of these needs the permission named and **every permission of the roles
concerned, at their scope**. T53 checks them at every entry point; a
repository does not.

- **Granting or removing a role** (a binding, an invitation, JIT default
  roles): `member.manage`, `team.manage` or `service_account.manage` for the
  subject, `project.manage` for a project binding. An invitation is checked
  again when it is accepted: whoever sent it must still hold every permission
  of its roles, or it can no longer be used.
- **Acting on a member who holds a role** (suspending, removing, changing
  their roles): `member.manage`. The roles concerned are all those the member
  holds, directly or through a team. So only an Owner acts on an Owner, and
  an Admin, who lacks `billing.manage`, does not act on a Billing member.
  The same goes for a service account that holds a role
  (`service_account.manage`).
- **Adding a member to a team:** `team.manage`. The roles concerned are the
  team's, on the organization and on every project. Removing a member from a
  team needs `team.manage` only.
- **Group mappings:** `connection.manage`. The roles concerned are those of
  the mapped team, because everyone in the group joins it at sign-in.
- **Changing what a connection trusts** (its `issuer`, `client_id` or
  `group_claim`): `connection.manage`. The roles concerned are the
  connection's JIT default roles and those of the teams it is mapped to,
  because the new provider decides who gets them.
- **Creating a SCIM token:** `connection.manage`. The roles concerned are
  those of every team with source `scim` and of every member with source
  `scim`, because the token adds members to those teams and suspends those
  members. A SCIM token acts on nothing else (identity.md, SCIM).
- **Mapping a tracker identity:** `trackers.manage`. The roles concerned are
  all those the mapped user holds, because whoever is that person on the
  tracker starts goals and answers questions as the user. Removing a mapping
  needs `trackers.manage` only.
- **Creating a token:** a token is limited to its owner's permissions, and
  creating one for a service account (`token.manage`) concerns the roles of
  that service account. Its scopes stay within the highest class of the
  owner's permissions and within the token the call is made with, and a
  credential below `write` creates none. An OAuth grant creates none at all:
  a token would outlive the person disconnecting the app. For the same
  reason a grant creates no capacity provider token, join code or
  invitation link, and an instance admin's grant is not an instance admin.
- **An instance admin recovering access** (identity.md, Recovery) holds every
  permission on the organization they entered, in a session only, whatever
  their bindings. The entry is audited as `instance_admin.entered`.

Whoever later binds a role to a team that is mapped or has source `scim`
grants it to the identity provider as well; the first rule applies to them.
An ownerless token counts as holding every permission its scopes allow (see
Scopes). The server-side commands are outside these rules (see What every
entry point needs).

## Scopes

Tokens and OAuth grants carry scopes, an upper bound on top of the owner's
current permissions: a request may use a permission when the owner has it
**and** one of the scopes includes the permission's scope class
(`domain.ScopesAllow`). Scopes are ordered: `admin` includes `write`,
`write` includes `read`.

| Scope | Allows |
|-------|--------|
| `read` | permissions of class read: reading projects, goals, jobs, gates, workers, the organization and the audit log |
| `write` | also class write: creating goals, dispatching and canceling jobs, resolving gates, proposing knowledge, deciding proposals, onboarding projects |
| `admin` | also class admin: every `*.manage` permission |

An **OAuth grant** acts as its user (`user:<id>`, identity.md), so roles and
gates apply as for a personal token, but it is an app, not the person at a
browser. Its access tokens work on `/mcp` only and are refused on `/api/v1`,
so nothing that is served there alone, and no rule that needs a session,
reaches it; `authz.Delegated` names such a caller for rules on `/mcp`, and
`authz.Engine.RefuseDelegated` refuses it (audited): `create_token`,
`create_provider`, `create_join_code` and `create_invitation`.

An **ownerless token** has no owner's permissions to meet, so it is limited
only by its scopes (`admin` on the rows from before migration 00005), in its
organization, and acts as `mcp:<name>`. This is a migration state, not a
feature: see identity.md, Tokens.

## Catalog

| Permission | Class | Granted on | Allows |
|------------|-------|------------|--------|
| `org.read` | read | organization | see the organization, its members, teams, roles and service accounts |
| `org.manage` | admin | organization | rename the organization, change its slug and `max_token_ttl`, grant and remove the Owner role |
| `billing.manage` | admin | organization | billing and plans (S.3); the [budget fields](#budget-fields) of the fleet |
| `member.manage` | admin | organization | invite, suspend and remove members, change their organization role bindings, within [No escalation](#no-escalation) |
| `team.manage` | admin | organization | create, rename and delete teams, change their members and their bindings, within [No escalation](#no-escalation) |
| `role.manage` | admin | organization | create, edit and delete custom roles (enterprise) |
| `service_account.manage` | admin | organization | create, edit and disable service accounts and change their bindings |
| `connection.manage` | admin | organization | sign-in connections, group mappings, verified domains, SSO enforcement, SCIM tokens, within [No escalation](#no-escalation) |
| `token.manage` | admin | organization | create service account tokens; list and revoke other people's tokens |
| `audit.read` | read | organization | read the audit log |
| `worker.read` | read | organization | list workers, machines, capacity providers, shapes, images and fleet usage |
| `worker.manage` | admin | organization | add, move and revoke workers; capacity providers, join codes, shapes, the fleet policy and the organization's images (locations and machine types, shown to users as Manage machines), except [budget fields](#budget-fields) |
| `pool.manage` | admin | organization | worker pools |
| `config.read` | read | organization | configuration resources and change plans, within what the caller may read of each area, and the assistant |
| `config.apply` | write | organization | make change plans and ask for their approval; applying one also needs what each of its resources needs ([config-resources.md](config-resources.md#permissions)) |
| `project.create` | write | organization | onboard a repo as a project |
| `trackers.manage` | admin | organization | tracker connections, routes and identities, within [No escalation](#no-escalation) |
| `models.read` | read | organization | read model connections and assignments, never a key |
| `models.manage` | admin | organization | model connections, and the organization's assignment and policy |
| `models.authorize` | admin | organization | authorize a model connection for the private code of every project (`model_authorization` `<connection>@*`), with the second confirmation |
| `project.read` | read | project | a project, its profile, councils, assumptions, decisions, knowledge and improvement proposals |
| `project.manage` | admin | project | project settings and project role bindings |
| `models.assign` | admin | project | the model overrides of a project |
| `goal.create` | write | project | create goals and report bugs |
| `goal.read` | read | project | goals, specs, tasks and changes |
| `job.read` | read | project | jobs, their logs, test runs and artifacts |
| `job.dispatch` | write | project | dispatch an ad-hoc job |
| `job.cancel` | write | project | cancel a job |
| `gate.read` | read | project | pending gates |
| `council.start` | write | project | start a council, add a constraint to one |
| `assumption.resolve` | write | project | resolve an assumption |
| `knowledge.propose` | write | project | propose a knowledge change |
| `proposal.decide` | write | project | accept, reject or snooze an improvement proposal, or mark its setup done |
| `trackers.read` | read | project | tracker connections, routes and identities (on the organization), and the tickets waiting in the Inbox |
| `gate.approve.profile` | write | project | resolve `profile_approval` gates |
| `gate.approve.spec` | write | project | resolve `spec_approval` gates |
| `gate.approve.test_plan` | write | project | resolve `test_plan_approval` gates |
| `gate.approve.merge` | write | project | resolve `merge_approval` gates |
| `gate.approve.council` | write | project | resolve `council_decision` gates |
| `gate.approve.assumption` | write | project | resolve `assumption_review` gates |
| `gate.approve.knowledge` | write | project | resolve `knowledge_proposal` gates of sections that are not protected |
| `gate.approve.knowledge_protected` | write | project | resolve `knowledge_proposal` gates of protected sections |
| `gate.approve.access_request` | write | project | resolve `access_request` gates |
| `gate.approve.improvement` | write | project | resolve `improvement_proposal` gates; approving one whose action is a goal also needs `goal.create` |
| `gate.approve.needs_you` | write | project | resolve `needs_you` gates, playbook escalations and `ask_human` questions alike |

### Budget fields

These fields say what the organization may spend or use, so they need
`billing.manage`, and `worker.manage` alone cannot change them:

- `capacity_providers.limits.monthly_budget`
- `shapes.monthly_budget`
- `fleet_policy.quotas.*`: `concurrent_machines`, `monthly_minutes`,
  `monthly_budget`

A change to a provider, shape or policy that touches one of them needs
`billing.manage` for that field, on top of `worker.manage` for any other field it
changes. Creating a row counts as changing its budget fields when they are set.
`billing.manage` alone changes these fields and nothing else of the row, and
reads them without `worker.read`. Instance-wide rows stay with instance admins.

## Built-in roles

Organization roles: **Owner** (`owner`, everything), **Admin** (`admin`,
everything but `org.manage` and `billing.manage`), **Billing** (`billing`),
**Member** (`member`: reads every project, creates goals, proposes
knowledge) and **Viewer** (`viewer`: reads every project). Project roles:
**Project admin** (`project_admin`: every project-scoped permission),
**Maintainer** (`maintainer`: runs the project and resolves every gate,
without `project.manage`), **Contributor** (`contributor`: creates goals and
jobs, starts councils, resolves spec, council, assumption, non-protected
knowledge and `needs_you` gates, but not merges, profiles, test plans,
protected knowledge, access requests or improvement proposals) and
**Viewer** (`viewer`: reads the project).

| Permission | Owner | Admin | Billing | Member | Viewer | Project admin | Maintainer | Contributor | Project viewer |
|------------|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|
| `assumption.resolve` | x | x |  |  |  | x | x |  |  |
| `audit.read` | x | x |  |  |  |  |  |  |  |
| `billing.manage` | x |  | x |  |  |  |  |  |  |
| `config.apply` | x | x | x | x |  |  |  |  |  |
| `config.read` | x | x | x | x | x |  |  |  |  |
| `connection.manage` | x | x |  |  |  |  |  |  |  |
| `council.start` | x | x |  |  |  | x | x | x |  |
| `gate.approve.access_request` | x | x |  |  |  | x | x |  |  |
| `gate.approve.assumption` | x | x |  |  |  | x | x | x |  |
| `gate.approve.council` | x | x |  |  |  | x | x | x |  |
| `gate.approve.improvement` | x | x |  |  |  | x | x |  |  |
| `gate.approve.knowledge` | x | x |  |  |  | x | x | x |  |
| `gate.approve.knowledge_protected` | x | x |  |  |  | x | x |  |  |
| `gate.approve.merge` | x | x |  |  |  | x | x |  |  |
| `gate.approve.needs_you` | x | x |  |  |  | x | x | x |  |
| `gate.approve.profile` | x | x |  |  |  | x | x |  |  |
| `gate.approve.spec` | x | x |  |  |  | x | x | x |  |
| `gate.approve.test_plan` | x | x |  |  |  | x | x |  |  |
| `gate.read` | x | x |  | x | x | x | x | x | x |
| `goal.create` | x | x |  | x |  | x | x | x |  |
| `goal.read` | x | x |  | x | x | x | x | x | x |
| `job.cancel` | x | x |  |  |  | x | x | x |  |
| `job.dispatch` | x | x |  |  |  | x | x | x |  |
| `job.read` | x | x |  | x | x | x | x | x | x |
| `knowledge.propose` | x | x |  | x |  | x | x | x |  |
| `member.manage` | x | x |  |  |  |  |  |  |  |
| `org.manage` | x |  |  |  |  |  |  |  |  |
| `org.read` | x | x | x | x | x |  |  |  |  |
| `models.assign` | x | x |  |  |  | x |  |  |  |
| `models.authorize` | x | x |  |  |  |  |  |  |  |
| `models.manage` | x | x |  |  |  |  |  |  |  |
| `models.read` | x | x |  | x | x |  |  |  |  |
| `pool.manage` | x | x |  |  |  |  |  |  |  |
| `project.create` | x | x |  |  |  |  |  |  |  |
| `project.manage` | x | x |  |  |  | x |  |  |  |
| `project.read` | x | x |  | x | x | x | x | x | x |
| `proposal.decide` | x | x |  |  |  | x | x |  |  |
| `role.manage` | x | x |  |  |  |  |  |  |  |
| `service_account.manage` | x | x |  |  |  |  |  |  |  |
| `team.manage` | x | x |  |  |  |  |  |  |  |
| `token.manage` | x | x |  |  |  |  |  |  |  |
| `trackers.manage` | x | x |  |  |  |  |  |  |  |
| `trackers.read` | x | x |  | x | x | x | x | x | x |
| `worker.manage` | x | x |  |  |  |  |  |  |  |
| `worker.read` | x | x |  | x | x |  |  |  |  |

Built-in roles are the same in every organization and change only with a
migration (and this table). Custom roles (enterprise, `role.manage`) are
validated by `domain.Role.Validate`: permissions from the catalog only, and
project-scoped ones only in a project role.

## What every entry point needs

Every `/api/v1` route (`api.RouteAccess`) and every MCP tool
(`mcp.ToolAccess`) has one entry; tests fail when a route or a tool has
none. "Signed in" means any active member, service account or token of the
organization, with no permission; "own" means the caller's own records need
no role and other people's need the one named, while a token or OAuth grant
still has to reach them: reading them needs class read, and revoking one of
your tokens needs class write, except the token you are calling with, which
can always end itself; "the gate's" means
`domain.GatePermission` of the gate's kind on its project. A tool that names
its object by ID (a job, a gate, an artifact, a council, a note) looks the
object up and checks the permission on the object's project.

| MCP tool | REST route | Needs |
|----------|------------|-------|
| | `POST /api/v1/session`, `DELETE /api/v1/session` | public |
| | `GET /api/v1/me`, `PUT /api/v1/session/organization`, `DELETE /api/v1/sessions`, `GET /api/v1/sessions`, `DELETE /api/v1/sessions/{session}`, `GET /api/v1/identities`, `POST /api/v1/cli/authorize` | signed in; `DELETE /sessions` and the last four work on the calling person's own records and need a browser session (`403` for a token of any scope), and `cli/authorize` needs one too |
| | `POST /api/v1/cli/token` | public: a code from `cli/authorize` and its PKCE verifier |
| | `POST /api/v1/auth/discover` | public, limited per address |
| | `POST /auth/setup` | public: the token of a setup link is the credential, and every token that does not work gets the same `404` |
| `onboard_project` | `POST /api/v1/projects` | `project.create` |
| `list_projects` | `GET /api/v1/projects` | `project.read` |
| `get_project` | `GET /api/v1/projects/{project...}` | `project.read` |
| `create_goal` | `POST /api/v1/goals` | `goal.create` |
| `report_bug` | `POST /api/v1/bugs` | `goal.create` |
| `list_goals` | `GET /api/v1/goals` | `goal.read` |
| `get_goal` | `GET /api/v1/goals/{goal}` | `goal.read`; its gates only with `gate.read` |
| `dispatch_job` | `POST /api/v1/jobs` | `job.dispatch` |
| `list_jobs` | `GET /api/v1/jobs` | `job.read` |
| `job_status` | `GET /api/v1/jobs/{job}` | `job.read` |
| `job_logs` | `GET /api/v1/jobs/{job}/logs` | `job.read` |
| `cancel_job` | `POST /api/v1/jobs/{job}/cancel` | `job.cancel` |
| `list_artifacts` | `GET /api/v1/jobs/{job}/artifacts` | `job.read` |
| | `GET /api/v1/artifacts/{id}` | `job.read` on the artifact's job's project |
| `list_test_runs` | `GET /api/v1/test-runs` | `job.read` |
| `verification_report` | `GET /api/v1/changes/{change}/verification` | `job.read` on the project of the change (a pull request number, a pull request URL or a task ref), like the test runs it is built from |
| `project_quality` | `GET /api/v1/quality` | `job.read` on the project, like the test runs it is built from |
| `list_assumptions` | `GET /api/v1/assumptions` | `project.read` |
| `resolve_assumption` | `POST /api/v1/assumptions/{assumption}/resolve` | `assumption.resolve` |
| `list_decisions` | `GET /api/v1/decisions` | `project.read` |
| `list_proposals` | `GET /api/v1/proposals` | `project.read` |
| `decide_proposal` | `POST /api/v1/proposals/{proposal}/decide` | `proposal.decide`, and a person for every decision: `accept` and `reject` resolve the proposal's gate, so they also need `gate.approve.improvement`, and accepting a proposal whose action is a goal also needs `goal.create`, as does `approve` on its gate; `snooze` hides the gate and `done` closes the proposal as carried out, which a service account, an agent or a worker never does |
| `get_trace` | `GET /api/v1/trace` | `project.read` on the project of the record and of its goal; a goal, spec, task or change ref, and the goal, spec, task, change and merge nodes, need `goal.read`; verification nodes need `job.read`. Without `goal.read` a council, decision or assumption ref gives only the councils, decisions and assumptions of its goal. Records of another project than the goal's are left out |
| `search_knowledge` | `GET /api/v1/knowledge` | `project.read`; a caller that is no person sees only the sections open to every role |
| `delete_note` | `DELETE /api/v1/knowledge/notes/{note}` | `knowledge.propose`, the closest write permission: deleting an agent note curates knowledge, and the catalog has no narrower one |
| `list_knowledge` | `GET /api/v1/knowledge/entries` | `project.read`; a caller that is no person (a service account, an agent) sees only the sections open to every role |
| `list_knowledge_proposals` | `GET /api/v1/knowledge/proposals` | `project.read`; the gate only with `gate.read`; proposals to a closed section only for a person |
| `propose_knowledge` | `POST /api/v1/knowledge/proposals` | `knowledge.propose` and, for a protected section, `gate.approve.knowledge_protected`, because the caller is recorded as its approver; so a service account proposes to unprotected sections only |
| `start_council` | `POST /api/v1/councils` | `council.start` |
| `list_councils` | `GET /api/v1/councils` | `project.read`; `gate_id` only with `gate.read` |
| `get_council` | `GET /api/v1/councils/{council}` | `project.read`; its gate and `gate_id` only with `gate.read` |
| `add_council_constraint` | `POST /api/v1/councils/{council}/constraints` | `council.start`; `gate_id` only with `gate.read` |
| `list_workers` | `GET /api/v1/workers` | `worker.read`; a running job shows only with `job.read` on its project |
| `list_machines` | `GET /api/v1/machines` | `worker.read`; a machine's job shows only with `job.read` on its project |
| `list_shapes` | `GET /api/v1/shapes` | `worker.read` |
| `list_providers` | `GET /api/v1/providers` | `worker.read` |
| `fleet_overview` | `GET /api/v1/fleet` | `worker.read`; jobs show only with `job.read` on their project |
| `fleet_usage` | `GET /api/v1/usage` | `worker.read`; the caller's organization only |
| `list_fleet_notices` | `GET /api/v1/fleet/notices` | `worker.read` and an instance admin: notices are the instance's, of every organization |
| `resolve_fleet_notice` | `POST /api/v1/fleet/notices/{notice}/resolve` | `worker.manage` and an instance admin |
| `list_images` | `GET /api/v1/images` | `worker.read` |
| | `POST /api/v1/images`, `DELETE /api/v1/images/{image}` | `worker.manage`, from a person only (a user, never a service account, an ownerless token or a machine); an instance-wide image also needs an instance admin. REST and CLI only, no MCP tool: an agent never registers an image others trust |
| `create_provider` | `POST /api/v1/providers` | `worker.manage`; only the caller's organization, and instance-wide only for an instance admin; never through an OAuth grant |
| `revoke_provider` | `POST /api/v1/providers/{provider}/revoke` | `worker.manage`; another organization's is `404`, an instance-wide one is for an instance admin |
| `create_join_code` | `POST /api/v1/join-codes` | `worker.manage`; as `create_provider`, so never through an OAuth grant |
| `list_join_codes` | `GET /api/v1/join-codes` | `worker.manage`; instance-wide codes show to an instance admin |
| `confirm_join` | `POST /api/v1/join-codes/{join}/confirm` | `worker.manage`; as `revoke_provider` |
| `list_pending_gates` | `GET /api/v1/gates` | `gate.read`; each gate says whether the caller may resolve it |
| `approve` | `POST /api/v1/gates/{gate}/approve` | the gate's |
| `request_changes` | `POST /api/v1/gates/{gate}/request-changes` | the gate's |
| `reject` | `POST /api/v1/gates/{gate}/reject` | the gate's |
| `list_tracker_connections` | `GET /api/v1/tracker-connections` | `trackers.read` on the organization |
| `create_tracker_connection` | `POST /api/v1/tracker-connections` | `trackers.manage` |
| `delete_tracker_connection` | `DELETE /api/v1/tracker-connections/{connection}` | `trackers.manage` |
| `discard_tracker_writes` | `POST /api/v1/tracker-connections/{connection}/discard-writes` | `trackers.manage` |
| `test_tracker_connection` | `POST /api/v1/tracker-connections/{connection}/test` | `trackers.manage`: it signs in to the tracker with the connection's token |
| `models_check` | `POST /api/v1/model-connections/{connection}/check` | `models.manage`: a control worker sends the connection's key to its endpoint |
| `models_test` | `POST /api/v1/model-connections/{connection}/tests` | `models.manage`: the test spends tokens of the connection |
| `models_results` | `GET /api/v1/model-checks` | `models.read` |
| `models_suggest` | `POST /api/v1/models/suggest` | `models.read`, and `config.apply` for the plan it makes; applying it needs what the plan needs |
| `list_tracker_routes` | `GET /api/v1/tracker-routes` | `trackers.read` on the organization |
| `create_tracker_route` | `POST /api/v1/tracker-routes` | `trackers.manage` |
| `delete_tracker_route` | `DELETE /api/v1/tracker-routes/{route}` | `trackers.manage` |
| `list_tracker_identities` | `GET /api/v1/tracker-identities` | `trackers.read` on the organization |
| `map_tracker_identity` | `POST /api/v1/tracker-identities` | `trackers.manage`, No escalation |
| `unmap_tracker_identity` | `DELETE /api/v1/tracker-identities/{identity}` | `trackers.manage` |
| `list_unmapped_tracker_people` | `GET /api/v1/tracker-people/unmapped` | `trackers.manage`: it reads the waiting tickets from their trackers |
| `list_tickets_waiting` | `GET /api/v1/tickets/waiting` | `trackers.read`: on the ticket's project, or on the organization for a ticket without one |
| `accept_ticket` | `POST /api/v1/tickets/{ticket}/accept` | `goal.create` on the project, and only a person: the goal starts as them |
| `dismiss_ticket` | `POST /api/v1/tickets/{ticket}/dismiss` | `goal.create` on the ticket's project, or on the organization for a ticket without one; with a gate, the gate's |
| `list_tokens` | `GET /api/v1/tokens` | own; `token.manage` for everyone's |
| `revoke_token` | `POST /api/v1/tokens/{token}/revoke` | own, with class write unless it is the token in use; `token.manage` for another's, which is `404` without it, as for a token that does not exist |
| `list_roles` | `GET /api/v1/roles` | `org.read` |
| `list_teams` | `GET /api/v1/teams` | `org.read` |
| `create_team` | `POST /api/v1/teams` | `team.manage` |
| `update_team` | `PATCH /api/v1/teams/{team}` | `team.manage` |
| `delete_team` | `DELETE /api/v1/teams/{team}` | `team.manage` and every permission of the team's roles, because it removes them |
| `list_team_members` | `GET /api/v1/teams/{team}/members` | `org.read` |
| `add_team_member` | `POST /api/v1/teams/{team}/members` | `team.manage`, No escalation |
| `remove_team_member` | `DELETE /api/v1/teams/{team}/members/{user}` | `team.manage` |
| `list_role_bindings` | `GET /api/v1/role-bindings` | `org.read`; with a project, `project.read` on it. Bindings on projects where the caller lacks `project.read` are left out |
| `create_role_binding` | `POST /api/v1/role-bindings` | by its subject and scope, No escalation |
| `delete_role_binding` | `DELETE /api/v1/role-bindings/{binding}` | by its subject and scope, No escalation |
| `list_service_accounts` | `GET /api/v1/service-accounts` | `org.read` |
| `create_service_account` | `POST /api/v1/service-accounts` | `service_account.manage` |
| `update_service_account` | `PATCH /api/v1/service-accounts/{service_account}` | `service_account.manage` |
| `set_service_account_disabled` | `POST /api/v1/service-accounts/{service_account}/disable`, `.../enable` | `service_account.manage`, No escalation |
| `list_members` | `GET /api/v1/members` | `org.read`; emails only with `member.manage`; a project the caller cannot read is not named |
| `set_member_suspended` | `POST /api/v1/members/{user}/suspend`, `.../resume` | `member.manage`, No escalation; ends the member's sessions, personal tokens and OAuth grants in the organization |
| `remove_member` | `DELETE /api/v1/members/{user}` | `member.manage`, No escalation; ends the member's sessions, personal tokens and OAuth grants in the organization |
| `update_organization` | `PATCH /api/v1/organization` | `org.manage` |
| `list_audit` | `GET /api/v1/audit` | `audit.read` |
| `list_connections` | `GET /api/v1/connections` | `connection.manage` |
| `create_connection` | `POST /api/v1/connections` | `connection.manage`; never sets JIT or a group claim, so No escalation has nothing to cover |
| `set_connection_enabled` | `POST /api/v1/connections/{connection}/enable`, `.../disable` | `connection.manage`; enabling needs a test sign-in that worked |
| | `GET /auth/{connection}/login?test=1` | a session with `connection.manage` in the connection's organization |
| `create_token` | `POST /api/v1/tokens` | own; `token.manage` for a service account's; never through an OAuth grant |
| `list_connected_apps` | `GET /api/v1/connected-apps` | own, class read; nobody sees another person's |
| `revoke_connected_app` | `POST /api/v1/connected-apps/{app}/revoke` | own, with class write unless it is the app in use; another person's is `404` |
| `create_invitation` | `POST /api/v1/invitations` | `member.manage`; never through an OAuth grant |
| `list_invitations` | `GET /api/v1/invitations` | `member.manage` |
| `revoke_invitation` | `POST /api/v1/invitations/{invitation}/revoke` | `member.manage` |
| `config_schema` | `GET /api/v1/config/schema` | `config.read` |
| `config_get` | `GET /api/v1/config/resources` | `config.read`, and each area's read permission: what the caller cannot read is left out |
| `config_plan` | `POST /api/v1/config/plans` | `config.apply`, and the read permission of each area the plan touches |
| `config_apply` | `POST /api/v1/config/plans/{plan}/request-approval` | `config.apply`; it never applies, it puts the plan in the Inbox |
| | `POST /api/v1/config/plans/{plan}/apply` | `config.apply` and everything the plan's resources need; only a person; a plan of the assistant, an MCP client or an ownerless token, and any plan that lowers protection, only from a browser session |
| `config_revert` | `POST /api/v1/config/plans/{plan}/revert` | `config.apply`; the new plan is confirmed like any other |
| `config_export` | `GET /api/v1/config/export` | `config.read`, and each area's read permission |
| `config_import` | `POST /api/v1/config/import` | `config.apply`, as `config_plan` |
| `config_plans` | `GET /api/v1/config/plans` | `config.read`; plans the caller did not make and cannot read every part of are left out |
| `config_get_plan` | `GET /api/v1/config/plans/{plan}` | `config.read`, as `config_plans`; `404` otherwise |
| `config_cancel` | `POST /api/v1/config/plans/{plan}/cancel` | `config.apply` |
| | `PUT /api/v1/config/plans/{plan}/secrets/{secret}` | `config.apply` and what the plan needs; deliberately not an MCP tool, because what passes through a client passes through its model |
| `assistant_send` | `POST /api/v1/assistant/messages` | `config.read`; with a project, `project.read` on it |
| `assistant_get` | `GET /api/v1/assistant/conversations/{conversation}` | `config.read`; own conversations only, `404` for another's |
| `assistant_list` | `GET /api/v1/assistant/conversations` | `config.read`; own conversations only |
| `list_domains` | `GET /api/v1/domains` | `connection.manage` |
| `add_domain` | `POST /api/v1/domains` | `connection.manage` |
| `verify_domain` | `POST /api/v1/domains/{domain}/verify` | `connection.manage` |
| `delete_domain` | `DELETE /api/v1/domains/{domain}` | `connection.manage` |
| `set_sso_enforcement` | `PUT /api/v1/domains/{domain}/enforcement` | `connection.manage`; turning it on sets `max_token_ttl` to 7 days when it is NULL, and nothing else about the cap |
| `set_max_token_ttl` | `PUT /api/v1/organization/max-token-ttl` | `org.manage` |
| `set_connection_provisioning` | `PUT /api/v1/connections/{connection}/provisioning` | `connection.manage`, No escalation: the JIT default roles it had and gets, and those of the teams mapped to it |
| `list_group_mappings` | `GET /api/v1/group-mappings` | `connection.manage` |
| `add_group_mapping` | `POST /api/v1/group-mappings` | `connection.manage`, No escalation |
| `remove_group_mapping` | `DELETE /api/v1/group-mappings/{mapping}` | `connection.manage` |
| `list_scim_tokens` | `GET /api/v1/scim-tokens` | `connection.manage` |
| `create_scim_token` | `POST /api/v1/scim-tokens` | `connection.manage`, No escalation |
| `revoke_scim_token` | `POST /api/v1/scim-tokens/{token}/revoke` | `connection.manage` |
| `create_role` | `POST /api/v1/roles` | `role.manage` and every permission of the role, on the organization |
| `update_role` | `PATCH /api/v1/roles/{role}` | `role.manage` and every permission the role has and gets, on the organization, because the change reaches everyone bound to it |
| `delete_role` | `DELETE /api/v1/roles/{role}` | `role.manage` and every permission of the role |

For a binding on the organization, No escalation also concerns everything
the member or service account already holds; for a binding on a project, only
the role at that project, so a Project admin manages their project without
holding what its people hold elsewhere.

**GitHub commands** (`@forecrew` in issues and PRs, `webhook.CommandAccess`)
run as the user whose linked GitHub identity wrote the comment, matched by
GitHub user ID: `approve` and `changes` need the gate's permission, `status`
needs `gate.read`, `bug` (on an issue) needs `goal.create`. Without a linked user with the permission the command is
refused with a reply. An organization where nobody holds a role yet is not
enforced: there `github.allowlist` still decides, as it did before
organizations, and an unknown author is ignored without a reply.

**Tracker webhooks** (`POST /webhooks/tracker/{connection}`, and the `issues`
events of `/webhooks/github`) need no permission: the adapter checks the
signature, and an event only says which ticket to read. Who may start work is
decided from what `Get` returns ([tracker.md](tracker.md)).

**The CLI** reaches the server through `/api/v1`, so a remote command needs
what its route needs. The server-side commands (`forecrew migrate`,
`config`, `token create|list|revoke`, `worker add|move|revoke` and T52's
break-glass `user`, `org` and `session` commands, and `setup-link`) work on the database
directly with the operator's server access and no permission; they are
audited as `user:<operator>`.

### Adding a tool or a route

A task that adds an MCP tool or an `/api/v1` route adds its entry to
`mcp.ToolAccess` or `api.RouteAccess` and to the table above, with an
existing permission where one fits. A new permission goes into the catalog
(`domain`, this file), and into the built-in roles that should have it with a
migration that updates their `roles` rows; that is a contract change the
task must name.

## The job MCP gateway

The job MCP gateway at `/job-mcp` ([job-mcp.md](job-mcp.md)) is not part of
this catalog. It is authenticated by job tokens bound to one lease, not by
people: a call is limited by the role's `tools`, the lease and the job's
project, and audited as the job. Job tokens carry no permissions here, and
human tokens are refused there. An `ask_human` question it opens is a
`needs_you` gate that people resolve with `gate.approve.needs_you`.

## Names for a policy engine

Actions and resources are named like Cedar entities so a policy engine can
take over attribute rules later (4.7): `Action::"gate.approve.merge"`,
`Organization::"<id>"`, `Project::"<id>"`, `User::"<id>"`, `Team::"<id>"`,
`ServiceAccount::"<id>"` (`authz.EntityUID`).

## The engine

`authz.Engine` is the one check. `Authorize(ctx, subject, permission,
resource)` takes the actor, a permission and the organization or a project,
and answers nil or an error wrapping `authz.ErrDenied` (`403 forbidden`).

1. The resource must be in the actor's organization.
2. A token or OAuth grant must allow the permission's class
   (`domain.ScopesAllow`). A session has no scopes to meet.
3. An ownerless token stops here: it is allowed.
4. `Evaluator.IsAuthorized(request, bindings)` decides, from a
   `Request{Principal, Action, Resource}` and the bindings that apply
   (`RoleBindingRepo.Effective`).

The evaluator is the only part that knows about roles. `RoleEvaluator` is the
union of the bindings; a policy engine replaces it through
`Engine.WithEvaluator` for attribute rules, with the same shape as cedar-go's
`IsAuthorized(entities, request)`, and no caller changes. What the engine reads
is kept for one request (`Engine.Request`) and never across requests, so a
change of bindings counts from the next request.

Three places call it. `guard` in `internal/api` and the wrapper in
`register` in `internal/mcp` check the entry of `RouteAccess` or `ToolAccess`
before the call: the caller holds the permission on the organization or on
some project. They do not know the project yet; `internal/tools` does, and
checks the permission on it, the gate's permission, own records and No
escalation. A route or a tool without an entry does not start, and a GitHub
command without one does not parse.

Denied write actions are audited as `access.denied` with the actor, its
credential, the permission and the resource. Changes to teams, role bindings,
service accounts and tokens are audited with their target.
