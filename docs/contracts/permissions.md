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
  whatever role they hold. Until knowledge proposals are built, every
  `knowledge_proposal` gate counts as protected.
- **The one exception on gates:** a PR merged or closed on GitHub resolves
  the `profile_approval`, `merge_approval` and `knowledge_proposal` gates
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
  subject, `project.manage` for a project binding.
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
- **Creating a token:** a token is limited to its owner's permissions, and
  creating one for a service account (`token.manage`) concerns the roles of
  that service account.

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
| `project.create` | write | organization | onboard a repo as a project |
| `project.read` | read | project | a project, its profile, councils, assumptions, decisions, knowledge and improvement proposals |
| `project.manage` | admin | project | project settings and project role bindings |
| `goal.create` | write | project | create goals and report bugs |
| `goal.read` | read | project | goals, specs, tasks and changes |
| `job.read` | read | project | jobs, their logs, test runs and artifacts |
| `job.dispatch` | write | project | dispatch an ad-hoc job |
| `job.cancel` | write | project | cancel a job |
| `gate.read` | read | project | pending gates |
| `council.start` | write | project | start a council, add a constraint to one |
| `assumption.resolve` | write | project | resolve an assumption |
| `knowledge.propose` | write | project | propose a knowledge change |
| `proposal.decide` | write | project | accept, reject or snooze an improvement proposal |
| `gate.approve.profile` | write | project | resolve `profile_approval` gates |
| `gate.approve.spec` | write | project | resolve `spec_approval` gates |
| `gate.approve.test_plan` | write | project | resolve `test_plan_approval` gates |
| `gate.approve.merge` | write | project | resolve `merge_approval` gates |
| `gate.approve.council` | write | project | resolve `council_decision` gates |
| `gate.approve.assumption` | write | project | resolve `assumption_review` gates |
| `gate.approve.knowledge` | write | project | resolve `knowledge_proposal` gates of sections that are not protected |
| `gate.approve.knowledge_protected` | write | project | resolve `knowledge_proposal` gates of protected sections |
| `gate.approve.access_request` | write | project | resolve `access_request` gates |
| `gate.approve.improvement` | write | project | resolve `improvement_proposal` gates |
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
protected knowledge, access requests or improvement proposals; every
knowledge gate counts as protected for now, so none of them in practice) and
**Viewer** (`viewer`: reads the project).

| Permission | Owner | Admin | Billing | Member | Viewer | Project admin | Maintainer | Contributor | Project viewer |
|------------|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|
| `assumption.resolve` | x | x |  |  |  | x | x |  |  |
| `audit.read` | x | x |  |  |  |  |  |  |  |
| `billing.manage` | x |  | x |  |  |  |  |  |  |
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
| `pool.manage` | x | x |  |  |  |  |  |  |  |
| `project.create` | x | x |  |  |  |  |  |  |  |
| `project.manage` | x | x |  |  |  | x |  |  |  |
| `project.read` | x | x |  | x | x | x | x | x | x |
| `proposal.decide` | x | x |  |  |  | x | x |  |  |
| `role.manage` | x | x |  |  |  |  |  |  |  |
| `service_account.manage` | x | x |  |  |  |  |  |  |  |
| `team.manage` | x | x |  |  |  |  |  |  |  |
| `token.manage` | x | x |  |  |  |  |  |  |  |
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
| | `GET /api/v1/me` | signed in |
| `onboard_project` | `POST /api/v1/projects` | `project.create` |
| `list_projects` | `GET /api/v1/projects` | `project.read` |
| `get_project` | `GET /api/v1/projects/{project...}` | `project.read` |
| `create_goal` | `POST /api/v1/goals` | `goal.create` |
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
| `verification_report` | `GET /api/v1/changes/{change}/verification` | `job.read`, like the test runs it is built from |
| `list_assumptions` | `GET /api/v1/assumptions` | `project.read` |
| `resolve_assumption` | `POST /api/v1/assumptions/{assumption}/resolve` | `assumption.resolve` |
| `list_decisions` | `GET /api/v1/decisions` | `project.read` |
| `search_knowledge` | `GET /api/v1/knowledge` | `project.read` |
| `delete_note` | `DELETE /api/v1/knowledge/notes/{note}` | `knowledge.propose`, the closest write permission: deleting an agent note curates knowledge, and the catalog has no narrower one |
| `propose_knowledge` | `POST /api/v1/knowledge/proposals` | `knowledge.propose` and, for a protected section the caller approves, `gate.approve.knowledge_protected` |
| `start_council` | `POST /api/v1/councils` | `council.start` |
| `list_councils` | `GET /api/v1/councils` | `project.read` |
| `get_council` | `GET /api/v1/councils/{council}` | `project.read`; its gate only with `gate.read` |
| `add_council_constraint` | `POST /api/v1/councils/{council}/constraints` | `council.start` |
| `list_workers` | `GET /api/v1/workers` | `worker.read` |
| `list_pending_gates` | `GET /api/v1/gates` | `gate.read`; each gate says whether the caller may resolve it |
| `approve` | `POST /api/v1/gates/{gate}/approve` | the gate's |
| `request_changes` | `POST /api/v1/gates/{gate}/request-changes` | the gate's |
| `reject` | `POST /api/v1/gates/{gate}/reject` | the gate's |
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

For a binding on the organization, No escalation also concerns everything
the member or service account already holds; for a binding on a project, only
the role at that project, so a Project admin manages their project without
holding what its people hold elsewhere.

**GitHub commands** (`@forecrew` in issues and PRs, `webhook.CommandAccess`)
run as the user whose linked GitHub identity wrote the comment, matched by
GitHub user ID: `approve` and `changes` need the gate's permission, `status`
needs `gate.read`. Without a linked user with the permission the command is
refused with a reply. An organization where nobody holds a role yet is not
enforced: there `github.allowlist` still decides, as it did before
organizations, and an unknown author is ignored without a reply.

**The CLI** reaches the server through `/api/v1`, so a remote command needs
what its route needs. The server-side commands (`forecrew migrate`,
`config`, `token create|list|revoke`, `worker add|move|revoke` and T52's
break-glass `user`, `org` and `session` commands) work on the database
directly with the operator's server access and no permission; they are
audited as `user:<operator>`.

**Planned tools** ([mcp.md](../mcp.md#planned-tools)) need, when their task
builds them: `list_proposals` `project.read`; `report_bug` `goal.create`; `decide_proposal` `proposal.decide`.

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
