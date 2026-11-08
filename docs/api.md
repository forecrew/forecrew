# REST API

forecrewd serves a JSON API under `/api/v1/` on its public URL. It is the
same service as the [MCP tools](mcp.md): one endpoint per tool, with the same
inputs, outputs and rules. The web UI and the `forecrew` CLI use it.
Implemented by `internal/api`.

Bodies are JSON (`Content-Type: application/json`), times are RFC 3339 in
UTC, IDs are UUID strings. Everything is scoped to the organization of the token or session.
Goals, jobs, tasks and gates are named by human ref (`GOAL-3`, `JOB-41`,
`TASK-12`, `GATE-7`) or UUID; projects by name or `owner/name`.

## Authentication

Organizations, people, sessions and tokens are described in
[contracts/identity.md](contracts/identity.md); what each endpoint needs is
in [contracts/permissions.md](contracts/permissions.md#what-every-entry-point-needs).
The organization is stored as an account, so answers name it `account`
(with its `slug`).

**Tokens.** Scripts, the CLI and MCP clients send a personal or service account
token (`fcm_...`) as `Authorization: Bearer <token>`. A token belongs to one
organization and one owner, has scopes and an expiry, and stops working when
its owner is disabled or is no longer an active member. Create one with
`POST /api/v1/tokens`; see [runbooks/mcp-token.md](runbooks/mcp-token.md).

OAuth access tokens (`fca_...`, docs/mcp.md) are for `/mcp` and get `401` here.

**Sessions.** People sign in through a connection, in the browser:

- `POST /api/v1/auth/discover` (public) takes `{"org": "<slug>", "email": "...",
  "invite": "fci_..."}`, all optional, and answers which connections the sign-in
  page shows: the instance's, plus those of the organization the input points
  at. `redirect` is set when the email's domain enforces one connection.
- `GET /auth/<connection>/login` (optional `org`, `invite` and `redirect`, a path of
  this site to return to) sends the browser
  to the provider; `GET /auth/<connection>/callback` finishes the sign-in and
  redirects to `/`. These are outside `/api/v1`, and a refusal is an HTML page.
- `POST /auth/setup` (public) takes `{"token": "fcu_..."}`, the token of the
  setup link `forecrew setup-link` printed (in the part of the link after `#`),
  and answers `{"organization": "<name>"}` with the `HttpOnly` cookie `fc_setup`
  (15 minutes, path `/auth/`) that the next sign-in spends to make its person an
  first Owner of that organization. A token that is wrong, expired or used, or a
  link of an organization that has an Owner or members already, gets the same
  `404` with `{"error": "..."}` (and drops the cookie). It does not use the link up:
  only the sign-in does. A body that is not `application/json` gets `415`.
- A sign-in resolves to a user by identity, then by verified email (only
  through an instance connection or an organization's connection for a domain
  it verified), then by the invitation of the link. Anyone else is refused.
- The answer sets the cookie `forecrew_session` (`HttpOnly`, `Secure`,
  `SameSite=Strict`, `Path=/`). The session ends after `auth.session_idle`
  without use and `auth.session_absolute` after sign-in, and at once when the
  user is disabled or leaves or is suspended from the current organization.

A session has a current organization. With several memberships and none named at
sign-in it has none until `PUT /api/v1/session/organization` picks one; until
then only `/me` and the session endpoints work, and the rest answers `403`
`no_organization`. Instance admins (`auth.instance_admins`) may switch to any
organization to recover access; that is audited as `instance_admin.entered`.
A session from an organization's own connection stays in that organization: it
cannot name another at sign-in or switch to one, and only accepts that
organization's invitations.

`DELETE /api/v1/session` signs out and needs no credential; `DELETE
/api/v1/sessions` ends every session of the caller; it, the session and sign-in lists and `cli/authorize` work for a browser session only, and a token of any scope gets `403`. `GET /api/v1/me` shows who
the caller is. These answers carry `Cache-Control: no-store`. A cookie request
that gets `401` also clears the cookie.

A request with the `Authorization` header never uses the cookie. With only the
cookie, every request that is not `GET` or `HEAD` must be sent as
`Content-Type: application/json` (also a `DELETE` or a `POST` without a body),
else it is refused with `415`, and its `Origin`, when present, must be the
public URL or the host of the request, else `403` `bad_origin`.

**Testing a connection.** `GET /auth/<connection>/login?test=1` in the browser of
an administrator (a session with `connection.manage` in the connection's
organization) signs in through a connection of the organization that is still
turned off. Nobody is signed in: the page shows the name and email the provider
answered, and the test is audited as `connection.tested`, with a fingerprint of
the kind, issuer, client ID and client secret reference it worked for. That is
what allows turning the connection on, by `set_connection_enabled` or by a
config plan. Changing any of those four takes the test away (a config plan
that does so audits `connection.test_cleared`), so the connection must be
tested again; the group claim, JIT and `enabled` do not.

A config plan follows the same rule: it is refused with `409` when it creates a
connection with `enabled: true`, turns one on, or changes one of those four
fields of a connection that stays on, unless the connection has a test for the
values the plan leaves it with. A plan may create a connection turned off,
change one that is off, and turn one off; to change the issuer of one that is
on, save the plan with `enabled: false`, test it, then turn it on. A connection
already on that changes none of the four stays on, tested or not. Turning on
a connection that is on, or off one that is off, changes nothing and answers `200`.

Connections from before this rule have no test of this kind: they show "Not
tested" (`tested: false`) until they are tested again. One that is on stays on.

**The CLI's login.** `forecrew login --server <url>` listens on `127.0.0.1`, opens
`/cli-login?redirect_uri=...&state=...&code_challenge=...` in the browser, where the
signed-in person sees what is being allowed and confirms. The page calls
`POST /api/v1/cli/authorize` and sends the browser to the CLI, which redeems
the code with `POST /api/v1/cli/token` and keeps the token. A machine whose
browser cannot reach the CLI pastes the code the page shows.

**Token login (legacy).** With `auth.token_login` on, `POST /api/v1/session`
takes `{"token": "fcm_..."}` as `application/json` and keeps the token in the
cookie. It is off by default and answers `403` `token_login_disabled`; see
[runbooks/upgrade.md](runbooks/upgrade.md).

## Errors

Non-2xx responses have this body:

```json
{"error": {"code": "not_found", "message": "job JOB-9: not found"}}
```

| Status | `code` | When |
|--------|--------|------|
| 400 | `bad_request` | Malformed JSON, unknown or mistyped fields, bad query parameter, missing `token` on login. |
| 400 | `invalid_input` | The tool refused the input: a field is missing or malformed, a bad ref, a decision the gate does not allow (`request-changes` on a `needs_you` gate, or without `feedback`). |
| 401 | `unauthorized` | Missing, unknown, expired or revoked token, or a session that ended. |
| 403 | `forbidden` | The caller lacks the permission, on the project the call is about or at all; the message names it. See [contracts/permissions.md](contracts/permissions.md). |
| 403 | `bad_origin`, `no_organization`, `token_login_disabled`, `sso_required` | See [Authentication](#authentication). |
| 404 | `not_found` | Unknown project, goal, job, gate or active token, or one of another account; unknown endpoint. |
| 409 | `conflict` | The state does not allow it, for example a gate that is already decided, a job that is not queued or running (`cancel`), a name that is taken, a binding that exists or the last Owner. |
| 413 | `too_large` | Body over 1 MiB. |
| 415 | `unsupported_media_type` | A cookie session sent a state-changing request that is not `application/json`. |
| 500 | `internal` | Server error; the cause is only in the server log. |
| 503 | `unavailable` | The feature is not configured: downloading an artifact while no `evidence` storage is configured. |

## Endpoints

Lists are JSON arrays (`[]` when empty). Field names are those of the
structs in `internal/domain` and `internal/mcp`.

| Method and path | Tool | Request | Response |
|-----------------|------|---------|----------|
| `POST /api/v1/projects` | `onboard_project` | `{"repo": "owner/name", "name": "optional", "default_branch": "optional, default main", "refresh": "optional, true runs only the test part again, for an onboarded project"}` | `202` `{"project": {...}, "run_id": "uuid"}` |
| `GET /api/v1/projects` | `list_projects` | | `200` projects |
| `GET /api/v1/projects/{project}` | `get_project` | `{project}` is the name or `owner/name` | `200` project with status and approved `profile` |
| `POST /api/v1/goals` | `create_goal` | `{"project", "title", "body", "platforms": ["web"], "councils": ["product"]}`; `councils` is optional, `[]` means none (no triage) | `201` goal (draft) |
| `POST /api/v1/bugs` | `report_bug` | `{"project", "title", "body", "platforms": ["web"]}` | `201` goal (draft, `kind: bug`); `400` when the project is in neither `testing.projects` nor `goal_v3.projects` |
| `GET /api/v1/goals?project=&status=` | `list_goals` | both filters optional | `200` goals |
| `GET /api/v1/goals/{goal}` | `get_goal` | | `200` goal detail, see below; `gates` is empty without `gate.read` |
| `POST /api/v1/jobs` | `dispatch_job` | `{"project", "role", "base_ref", "branch", "needs": [], "pool", "include_repo_facts", "goal", "task", "feedback", "prompt"}`; `project` and `role` required, the role must be read-only; `pool` defaults to the role's pool and its policy must allow the role (`400 invalid_input` otherwise) | `201` job (queued, with its `pool`) |
| `GET /api/v1/jobs?project=&goal=&status=&limit=` | `list_jobs` | all optional; `limit` defaults to 50, at most 500 | `200` jobs, newest first |
| `GET /api/v1/jobs/{job}` | `job_status` | | `200` job with `ref`, `reason` (why a queued job waits, a sentence; why a failed job failed), `reason_code` (on a job that waits, see [mcp.md](mcp.md#why-a-job-waits)) and `worker_name`; `context_manifest` for an agent job built with a context pack: what the pack held (`budget`, `size`, and `sections` with `name`, `size`, `items` and `dropped`, each item with `id` and `path` as they apply and `size`, in characters); for a command job also `tests`, the test runs of its latest attempt added up: `runs`, `status`, `passed`, `failed`, `skipped`, `errored` |
| `GET /api/v1/jobs/{job}/logs?after_seq=&limit=` | `job_logs` | `limit` defaults to 200, at most 1000 | `200` `{"events": [...], "next_seq": 12}`; pass `next_seq` as `after_seq` to continue |
| `POST /api/v1/jobs/{job}/cancel` | `cancel_job` | body optional | `200` job (now `canceled`) with `ref`; `409` when the job is not queued or running |
| `GET /api/v1/jobs/{job}/artifacts` | `list_artifacts` | | `200` artifacts of the job by name: `id`, `name`, `content_type`, `size`, `sha256`, `created_at` |
| `GET /api/v1/test-runs?job=&project=&change=&sha=&limit=` | `list_test_runs` | at least one of `job`, `change` (a PR number, needs `project`) and `sha`; `limit` defaults to 100, at most 500 | `200` test runs of command jobs, oldest first: `id`, `job_id`, `job_ref`, `change_id`, `sha`, `suite`, `environment`, `cell`, `status`, `counts`, `duration_ms`, `exit_code`, `versions`, `report_artifacts`, `attempt`, `created_at` and `failures` (the cases that failed or errored, at most 100 per run: `name`, `classname`, `status`, `message`, written by the repo's tests and so untrusted data) and `failures_truncated` |
| `GET /api/v1/changes/{change}/verification?project=` | `verification_report` | `{change}` is a PR number (needs `project`), a PR URL (path-escaped) or a task ref such as `TASK-12` (its newest PR) | `200` the report: `id`, `change` (PR number), `url`, `sha`, `status` (`passed`, `failed`, `incomplete`), `current` (the report is for the PR's head commit; else it is the latest and stale), `report` (see below), `artifact_id` (the rendered Markdown, download it with `GET /api/v1/artifacts/{id}`; absent without evidence storage or after retention), `created_at`, `updated_at`; `404` when the PR is unknown or has no report yet |
| `GET /api/v1/quality?project=` | `project_quality` | `project` required | `200` `{"project", "default_branch", "baseline", "changes"}`: `baseline` is `{"sha", "cells"}` for the newest commit with runs outside a pull request and outside base checks, leaving out the heads of open pull requests (a run records no branch, so it is the latest run outside a pull request, not necessarily on the default branch; absent when none); `changes` are the open pull requests, newest first, at most 50: `number`, `url`, `branch`, `sha`, `task` (a ref, when there is one), `cells` of the head commit (the runs that count for its report) and `verification` (as the report above, absent when none). A cell is the latest attempt of a suite in a matrix cell of an environment: `suite`, `environment`, `cell`, `status`, `counts`, `job` (a ref) and `failures` (at most 100: `name`, `classname`, `status`, `message`, untrusted data) |
| `GET /api/v1/artifacts/{id}` | | `{id}` is an artifact UUID | `200` the file, streamed; see [Artifact downloads](#artifact-downloads). `404` when it is unknown, of another account, or deleted by retention; `503` without evidence storage |
| `GET /api/v1/knowledge?project=&q=&section=&kind=&provider=&limit=` | `search_knowledge` | `project` and `q` required; `kind` is `entry`, `note` or `generated`; `limit` defaults to 8, at most 50 | `200` results (an array, `[]` when none) as in [mcp.md](mcp.md): `id`, `kind`, `provider`, `section`, `path`, `title`, `source_url`, `snippet`, `untrusted`, `provenance`, `needs_reverify`, `rank`. Snippets are not wrapped here: check `untrusted` before handing them to an agent. A snippet marks the words that matched with `<b>` and `</b>`; any other markup in it is text |
| `DELETE /api/v1/knowledge/notes/{note}` | `delete_note` | `{note}` is a note UUID | `200` `{"note": "uuid", "message": "..."}`; `400` when the entry is not an agent note, `404` when unknown |
| `GET /api/v1/knowledge/entries?project=&section=&kind=&provider=&goal=&limit=` | `list_knowledge` | `project` required; `goal` with `kind=note` adds that goal's notes to the project's; `limit` defaults to 50, at most 200 | `200` `{"sections": [...], "entries": [...]}`. `sections`: `name`, `provider`, `provider_kind`, `instance`, `paths` (repo globs or the provider's locations), `owner`, `protected`; `notes` and `generated` are always there. `entries`, by section and path: the fields of a search result (`id`, `kind`, `provider`, `section`, `path`, `title`, `source_url`, `snippet`, `untrusted`, `provenance`, `needs_reverify`, notes' `scope`, `goal_id`, `tags`, `expires_at`) and `body`, the whole Markdown. Bodies are not wrapped here: check `untrusted`, and show them as text |
| `GET /api/v1/knowledge/proposals?project=&goal=&status=&limit=` | `list_knowledge_proposals` | `project` required; `status` is `queued`, `open`, `accepted` or `rejected`; `limit` defaults to 50, at most 200 | `200` proposals (an array), newest first: `ref`, `section`, `path`, `action`, `body`, `reason`, `proposed_by`, `status`, `batch`, `goal_ref`, `external_ref`, `url` (the pull request, for the repo provider) and `gate` (the `knowledge_proposal` gate a human decides, as in `GET /api/v1/gates`). `body` and `reason` are agent or human text: show them as text |
| `POST /api/v1/knowledge/proposals` | `propose_knowledge` | `{"project", "section", "path", "body", "reason", "action"}`; `action` is optional, as in [mcp.md](mcp.md) | `201` `{"proposal": "KB-5", "status": "open", "number": 12, "url": "..."}`; `400` for what the policy refuses |
| `POST /api/v1/councils` | `start_council` | `{"project", "question", "kind", "roles": [], "constraints": []}`; `project` and `question` required, `kind` defaults to `adhoc` | `202` council with `ref` and `run_id` (`running`) |
| `GET /api/v1/councils?project=&status=` | `list_councils` | both optional | `200` councils of the project, or of every project you may read, with `ref`, newest first, at most 200, without their briefs (`get_council` has them as untrusted data); no `gate_id` without `gate.read`. `403` for a project you cannot read |
| `GET /api/v1/councils/{council}` | `get_council` | `{council}` is `COUNCIL-2` or a UUID | `200` council with `ref`, `brief`, `contributions` by stage and round (`member`, `runtime`, `stage`, `round`, `content`, `evidence_check`; agent text, so untrusted data), `gate` and `decision`; without `gate.read` there is no `gate` or `gate_id` |
| `POST /api/v1/councils/{council}/constraints` | `add_council_constraint` | `{"constraint": "..."}` | `200` council; the next round's prompts include it. `409` when the council ended |
| `GET /api/v1/workers` | `list_workers` | | `200` workers with `pool`, `labels`, `version`, `last_seen_at`, `online` and `running_jobs` (refs) |
| `GET /api/v1/machines?state=&limit=` | `list_machines` | both optional; without `state` the machines that exist; `limit` defaults to 100, at most 500 | `200` machines with `state`, `name`, `shape`, `provider`, `job_ref`, `error` and their times |
| `GET /api/v1/shapes` | `list_shapes` | | `200` shapes offered to the organization with `provider` and `ownership` |
| `GET /api/v1/providers` | `list_providers` | | `200` providers with `online` and `ownership`. Capabilities are what an agent said, unchecked; they are not wrapped here |
| `GET /api/v1/fleet` | `fleet_overview` | | `200` the overview as in [mcp.md](mcp.md): `headline`, `working`, `waiting`, `spend`, `routing`, `locations`, `queue`, `running` |
| `GET /api/v1/usage?month=&records=` | `fleet_usage` | both optional: `month` `YYYY-MM` (UTC, default the current month), `records=true` lists every usage record | `200` the report as in [mcp.md](mcp.md): `from`, `to`, `currency`, `total`, `hosted`, `own`, `shapes`, `running` and, when asked, `records` |
| `GET /api/v1/fleet/notices?all=&limit=` | `list_fleet_notices` | `all=true` adds resolved notices; `limit` defaults to 100, at most 500 | `200` notices as in [mcp.md](mcp.md); `403` but for an instance admin |
| `POST /api/v1/fleet/notices/{notice}/resolve` | `resolve_fleet_notice` | | `200` the notice; `409` resolved already; `403` but for an instance admin |
| `GET /api/v1/images?name=&os=&arch=&format=&limit=` | `list_images` | all optional; `limit` defaults to 100, at most 500 | `200` images (an array) of the organization and the instance, newest build first within a name, each with `scope` |
| `POST /api/v1/images` | | a signed image manifest ([images.md](images.md)) and optional `"instance": true`: `{"name", "version", "os", "arch", "format", "uri", "digest", "signature", "built_at", "contents"}` | `201` the image. `403` unless the caller is a person with `worker.manage` (an instance admin for `instance`); `400` for an invalid or unsigned manifest; `409` when the name, version, arch and format exist. The server checks the form; the fleet agent checks the signature |
| `DELETE /api/v1/images/{image}` | | `{image}` is an image UUID | `200` the image removed. `404` when unknown or another organization's; `409` while a live machine uses it |
| `POST /api/v1/providers` | `create_provider` | `{"name": "home", "kind": "microvm", "org": "optional slug"}` | `201` `{provider, token}`; the token is shown this once |
| `POST /api/v1/providers/{provider}/revoke` | `revoke_provider` | `{provider}` is a name or UUID | `200` the provider, now `revoked` |
| `POST /api/v1/join-codes` | `create_join_code` | `{"org": "optional slug", "cloud": false}` | `201` `{join_code, code}`; the code is shown this once |
| `GET /api/v1/join-codes` | `list_join_codes` | | `200` codes with `status`, `kind`, `facts`, `facts_hash` and `expires_at`. Facts are what a machine said, unchecked; they are not wrapped here |
| `POST /api/v1/join-codes/{join}/confirm` | `confirm_join` | `{"name": "home", "facts_hash": "from list_join_codes"}` | `200` the new provider; `409` unless the code is `claimed` and its report is still the one named |
| `POST /api/v1/providers/{provider}/image-builds` | `start_image_build` | `{provider}` is a name or UUID; `{"version": "2026.11.1", "iso": "D:\\isos\\server.iso", "iso_checksum": "sha256:<hex>", "chocolatey_sha256": "<hex>", "windows_image_name": "optional", "license_confirmed": true}` | `202` the build, `requested`. `400` without the licence statement or with a bad checksum or path, or for a location that is not an active Hyper-V or Proxmox one; `403` unless a person with `worker.manage`; `409` while the location builds another image |
| `GET /api/v1/providers/{provider}/image-builds?limit=` | `list_image_builds` | `limit` defaults to 20, at most 100 | `200` the builds (an array), newest first, with `state`, `step`, `percent`, `error`, and `manifest` and `digest` when done. A finished build is unsigned and registers nothing |
| `GET /api/v1/gates?project=` | `list_pending_gates` | `project` optional | `200` pending gates with `ref`, `kind`, `project`, `goal_ref`, `task_ref`, `summary`, `url` (GitHub issue or PR), `can_resolve` (whether the caller may resolve it) and `created_at` |
| `POST /api/v1/gates/{gate}/approve` | `approve` | `{"feedback": "optional", "save_as_lesson": false}` (body optional; `save_as_lesson` needs `feedback`) | `200` the gate, now `approved` |
| `POST /api/v1/gates/{gate}/request-changes` | `request_changes` | `{"feedback": "required", "save_as_lesson": false}` | `200` the gate, now `changes_requested` |
| `POST /api/v1/gates/{gate}/reject` | `reject` | `{"feedback": "optional"}` (body optional) | `200` the gate, now `rejected` |
| `GET /api/v1/tokens` | `list_tokens` | | `200` tokens with `id`, `name`, `created_at`, `last_used_at`, `revoked_at`, `owner_kind`, `owner_id`, `scopes`, `expires_at` and `current` (the token of this request); no hash or secret |
| `POST /api/v1/tokens` | `create_token` | `{"name", "scopes": ["read"], "ttl": "720h", "service_account": "ci"}`; `ttl` defaults to `auth.token_default_ttl`, at most `8760h`, `service_account` to yourself | `201` `{"token": {...}, "secret": "fcm_..."}`; the secret is shown once. `403` for a scope above the owner's roles or above the token you use |
| `GET /api/v1/connected-apps` | `list_connected_apps` | | `200` your connected apps in the organization, the OAuth clients you authorized: `id`, `name`, `client`, `scopes`, `refreshed_at`, `current` |
| `POST /api/v1/connected-apps/{app}/revoke` | `revoke_connected_app` | `{app}` is the ID | `200` the app; its tokens stop working at once. `403` when a read-scoped credential disconnects an app; `404` when it is not an active app of yours |
| `POST /api/v1/tokens/{token}/revoke` | `revoke_token` | `{token}` is a name or ID; body optional | `200` `{"token": {...}, "self": false, "message": "..."}`; `403` when a read-scoped credential revokes another of your tokens (the token in use can always end itself); `404` when no active token has that name or ID, or it is someone else's and you lack `token.manage`. `self` is true when the caller revoked the token it used: the next request, with the header or the session cookie, gets `401` |
| `POST /api/v1/invitations` | `create_invitation` | `{"email" or "github_login", "roles": [{"role": "member"}, {"role": "maintainer", "project": "app"}], "ttl": "168h"}`; `ttl` defaults to 7 days, at most `720h`. A `github_login` is resolved to its GitHub user ID now, and only that account can accept; an unknown login is `400`, and `503` when GitHub cannot be reached | `201` `{"invitation": {...}, "url": "..."}`; the link is shown once |
| `GET /api/v1/invitations` | `list_invitations` | | `200` invitations, newest first |
| `POST /api/v1/invitations/{invitation}/revoke` | `revoke_invitation` | `{invitation}` is its ID; body optional | `200` the invitation |
| `POST /api/v1/auth/discover` | | `{"org", "email", "invite"}`, all optional | `200` `{"connections": [{"id", "kind", "name", "login_url"}], "redirect": "optional", "token_login": false}` |
| `POST /api/v1/session` | | `{"token": "fcm_..."}`; needs `auth.token_login` | `200` `me`, sets the cookie; `403` `sso_required` for a user under SSO enforcement |
| `DELETE /api/v1/session` | | | `204`, ends the session and clears the cookie |
| `DELETE /api/v1/sessions` | | | `204`, ends every session of the caller (`403` for a token) |
| `GET /api/v1/sessions` | | | `200` the active sessions of the calling person, newest first: `id`, `created_at`, `last_seen_at`, `expires_at`, `user_agent`, `ip`, `organization` (slug), `connection` (name) and `current`. A browser session only: a token of any scope gets `403` |
| `DELETE /api/v1/sessions/{session}` | | | `204`, ends one of the person's sessions (clearing the cookie when it is the one used); `404` when it is not theirs or is over |
| `GET /api/v1/identities` | | | `200` (browser session only, `403` for a token) the sign-ins linked to the calling person: `id`, `connection` (name), `kind`, `login`, `email`, `last_used_at`, `created_at` |
| `POST /api/v1/cli/authorize` | | `{"redirect_uri": "http://127.0.0.1:PORT/path", "state", "code_challenge", "organization"}`; a session only; `organization` is a slug, the session's own by default | `200` `{"redirect": "...?code=...&state=...", "code"}`: the browser goes to `redirect`, which the CLI listens on. The address must be a loopback IP with a port; the challenge is PKCE S256. `403` unless the person is an active member |
| `POST /api/v1/cli/token` | | `{"code", "code_verifier", "name"}`; public: the code and its verifier are the credential | `200` `{"secret", "token", "account", "user"}`: a personal token named after `name` (a number is added when it is taken) with the highest scope the person's roles allow. A code works once, within 5 minutes, and is kept in memory only; `400` for any other code |
| `PUT /api/v1/session/organization` | | `{"organization": "<slug>"}` | `200` `me`; `403` unless you are an active member |
| `GET /api/v1/me` | | | `200` session: `token` (for a token), `account` (the current organization; absent until a session picks one), `user` (absent for a service account or an ownerless token), `organizations` (`id`, `name`, `slug`, the ones the session can switch to), `enterprise` (true in the enterprise build), `roles` (`role`, `scope`, `project_id`, `team_id` when it comes through a team), `permissions` (held on the organization and, for project-scoped ones, on every project) and `projects` (by project ID, the project-scoped permissions on each project where the caller has any). A token shows what it can use, its scopes applied |
| `GET /api/v1/roles` | `list_roles` | | `200` roles with `name`, `scope`, `permissions`, `built_in`, `custom` |
| `POST /api/v1/roles` | `create_role` | `{"name", "scope", "description", "permissions"}` | `201` role; `409` when the name is taken |
| `PATCH /api/v1/roles/{role}` | `update_role` | `{"name", "description", "permissions"}`, each optional | `200` role |
| `DELETE /api/v1/roles/{role}` | `delete_role` | | `200` role; `409` while it is bound, in a pending invitation or a JIT default |
| `GET /api/v1/domains` | `list_domains` | | `200` domains with `domain`, `verified_at`, `enforce_sso`, `connection_id`, `record_name`, `record_value` |
| `POST /api/v1/domains` | `add_domain` | `{"domain"}` | `201` domain |
| `POST /api/v1/domains/{domain}/verify` | `verify_domain` | | `200` domain; `409` when the record is missing or another organization verified it |
| `DELETE /api/v1/domains/{domain}` | `delete_domain` | | `200` domain |
| `PUT /api/v1/domains/{domain}/enforcement` | `set_sso_enforcement` | `{"enforce", "connection"}` | `200` domain |
| `PUT /api/v1/organization/max-token-ttl` | `set_max_token_ttl` | `{"max_token_ttl"}`, a duration; empty removes the cap | `200` organization |
| `PUT /api/v1/connections/{connection}/provisioning` | `set_connection_provisioning` | `{"jit_enabled", "default_roles", "group_claim"}`, each optional | `200` `{"connection_id", "name", "jit", "group_claim"}` |
| `GET /api/v1/group-mappings` | `list_group_mappings` | | `200` mappings |
| `POST /api/v1/group-mappings` | `add_group_mapping` | `{"connection", "group", "team"}` | `201` mapping |
| `DELETE /api/v1/group-mappings/{mapping}` | `remove_group_mapping` | | `200` mapping |
| `GET /api/v1/scim-tokens` | `list_scim_tokens` | | `200` tokens, never their values |
| `POST /api/v1/scim-tokens` | `create_scim_token` | `{"name"}` | `201` `{"token", "secret", "url"}`; the secret is shown once |
| `POST /api/v1/scim-tokens/{token}/revoke` | `revoke_scim_token` | | `200` token |
| `GET /api/v1/teams` | `list_teams` | | `200` teams |
| `POST /api/v1/teams` | `create_team` | `{"name", "slug"}`; `slug` is derived from the name when left out | `201` team; `409` when the slug is taken |
| `PATCH /api/v1/teams/{team}` | `update_team` | `{team}` is a slug or UUID; `{"name", "slug"}`, both optional | `200` team |
| `DELETE /api/v1/teams/{team}` | `delete_team` | | `200` the team, deleted with its members and bindings |
| `GET /api/v1/teams/{team}/members` | `list_team_members` | | `200` members: `user_id`, `source`, `created_at` |
| `POST /api/v1/teams/{team}/members` | `add_team_member` | `{"user": "uuid"}` | `201` member; `404` when the user is not a member of the organization |
| `DELETE /api/v1/teams/{team}/members/{user}` | `remove_team_member` | | `200` member |
| `GET /api/v1/role-bindings?project=&subject_kind=&subject=` | `list_role_bindings` | all optional; `subject` needs `subject_kind` | `200` bindings with `id`, `subject_kind`, `subject_id`, `role`, `role_scope`, `project`, `subject_name` (the user, team or service account), without those on projects you cannot read |
| `POST /api/v1/role-bindings` | `create_role_binding` | `{"subject_kind", "subject", "role", "project"}`; `subject_kind` is `user`, `team` or `service_account`, `subject` a user UUID, a team slug or a service account name (or their UUIDs); without `project` the role applies to the organization | `201` binding; `409` when it exists |
| `DELETE /api/v1/role-bindings/{binding}` | `delete_role_binding` | | `200` the binding; `409` for the last Owner |
| `GET /api/v1/service-accounts` | `list_service_accounts` | | `200` service accounts |
| `POST /api/v1/service-accounts` | `create_service_account` | `{"name", "description"}` | `201` service account; `409` when the name is taken |
| `PATCH /api/v1/service-accounts/{service_account}` | `update_service_account` | `{service_account}` is a name or UUID; `{"name", "description"}`, both optional | `200` service account |
| `POST /api/v1/service-accounts/{service_account}/disable`, `.../enable` | `set_service_account_disabled` | body optional | `200` service account |
| `GET /api/v1/tracker-connections` | `list_tracker_connections` | | `200` connections |
| `POST /api/v1/tracker-connections` | `create_tracker_connection` | `{"name", "kind", "config", "triggers": {"assignee", "label", "state"}}` | `201` connection; `400` when the adapter refuses it; `409` when the name is taken |
| `POST /api/v1/tracker-connections/{connection}/discard-writes` | `discard_tracker_writes` | `{connection}` is a name or UUID; no body | `200` `{connection, writes}`: the unsent writes of tickets that are over, dropped; those of tickets at work stay |
| `DELETE /api/v1/tracker-connections/{connection}` | `delete_tracker_connection` | `{connection}` is a name or UUID | `200` the connection; `409` while it has live tickets or unsent writes (`discard-writes` drops those of ended tickets) |
| `POST /api/v1/tracker-connections/{connection}/test` | `test_tracker_connection` | `{connection}` is a name or UUID | `200` `{"connection", "ok", "checks": [{"name", "ok", "detail"}], "states": [{"name", "closed"}]}`: address, certificate, login, projects and transitions for `jira` and `linear`, then `routes`, the states of the connection's routes against the tracker's; a check after a failed one says it was not checked. A first test that passes stores what the tracker says of itself (Jira's deployment). `states` are the tracker's states or columns, given when every check passed: what a route's `states` can map to |
| `POST /api/v1/model-connections/{connection}/check` | `models_check` | `{connection}` is the name of an organization's model connection | `202` the check (`domain.ModelCheck`) with `status: queued` and its `job_id`: a `model_check` job on a control worker. Its result comes when the job ends (`GET /api/v1/model-checks`) |
| `POST /api/v1/model-connections/{connection}/tests` | `models_test` | `{"model", "run"}` | `200` `{"connection", "model", "max_turns", "timeout_seconds", "estimate": {"input_tokens", "output_tokens", "cost_usd", "basis", "worst_input_tokens", "worst_cost_usd", "worst_basis"}, "test"}`: without `run`, only the estimate, which a person sees before the test runs; with `run`, `test` is the started test with its `job_id`. `cost_usd` is the typical test, `worst_cost_usd` the worst case (15 turns × the model's full context window); each is absent when it cannot be known |
| `GET /api/v1/model-checks` | `models_results` | `?connection=` | `200` the last key check of each connection (`model` empty, `key`: `{status, detail, models, agent_version}`) and the last test of each of its models (`test`: `{abilities, level, tokens, duration_ms, turns, cost_usd, agent_version, error}`), `stale` when the agent changed since |
| `POST /api/v1/models/suggest` | `models_suggest` | `{"preset", "source"}`: `balanced` (default), `low_cost` or `local_only` | `200` `{"preset", "tiers", "roles", "notes", "plan"}`: the suggestion, what could not be filled, and the plan of the organization's `model_assignment` a person confirms; no `plan` when nothing changes |
| `GET /api/v1/models/catalog` | `models_catalog` | | `200` `{"makers", "providers", "models", "presets", "roles"}`: the model catalog this release ships (providers with `adapter`, `base_url`, `key_env`, `country`, `router`; models with `maker`, `tier`, `context`, `price`), the presets of `models_suggest`, and the roles it fills (`{name, tier, writes}`) |
| `GET /api/v1/tracker-routes?connection=` | `list_tracker_routes` | optional | `200` routes in order, with `connection` and `project`, and `warning` when the tracker did not take the last state Forecrew wrote (the tracker's words; editing the route clears it) |
| `POST /api/v1/tracker-routes` | `create_tracker_route` | `{"connection", "project", "position", "match": {"project", "types", "labels", "components"}, "goal_kind", "playbook", "states"}` | `201` route; `409` when the connection has a route at that `position` already |
| `DELETE /api/v1/tracker-routes/{route}` | `delete_tracker_route` | | `200` the route |
| `GET /api/v1/tracker-people/unmapped?connection=` | `list_unmapped_tracker_people` | optional | `200` `{"people": [{"connection", "external_id", "external_name", "tickets", "suggestions": [{"user", "name", "reason"}]}], "problems": []}`: who handed tickets over and maps to no user, with members of the same name; remembered for five minutes per connection |
| `GET /api/v1/tracker-identities?connection=` | `list_tracker_identities` | optional | `200` identities |
| `POST /api/v1/tracker-identities` | `map_tracker_identity` | `{"connection", "external_id", "external_name", "user"}` | `201` identity; `403` when the user holds more than you; `409` when mapped already |
| `DELETE /api/v1/tracker-identities/{identity}` | `unmap_tracker_identity` | | `200` the identity |
| `GET /api/v1/tickets/waiting?project=&queued=` | `list_tickets_waiting` | optional; `queued=true` adds the tickets queued under the limit of goals | `200` tickets with `status`, `reason`, `connection`, `project`, `gate_ref`, and `limit` (`trackers.max_active_goals`) on a queued one |
| `POST /api/v1/tickets/{ticket}/accept` | `accept_ticket` | `{"project"}`, needed when no route gave one; body optional | `200` ticket, `active` or `queued`; `409` when it is not waiting |
| `POST /api/v1/tickets/{ticket}/dismiss` | `dismiss_ticket` | `{"reason"}`; body optional | `200` ticket; `409` when it is not waiting |

`POST /webhooks/tracker/{connection}` takes the webhooks of a tracker, outside
`/api/v1`: the adapter checks the signature (`401`), and the event only says
which ticket to read ([contracts/tracker.md](contracts/tracker.md)).
| `GET /api/v1/members` | `list_members` | | `200` members, by name: `user_id`, `name`, `email` (only with `member.manage`), `status` (`active`, `invited`, `suspended`), `source`, `disabled`, `roles` (the roles bound to the member directly: `role`, `scope`, `project`; those of their teams are the teams'), `teams` (slugs), `last_seen_at`, `created_at`. A project the caller cannot read is not named |
| `POST /api/v1/members/{user}/suspend`, `.../resume` | `set_member_suspended` | `{user}` is the user UUID; body optional | `200` the member. Suspending ends their sessions and revokes their personal tokens in the organization in the same transaction; `409` for the last Owner, and for resuming a member who is not suspended |
| `DELETE /api/v1/members/{user}` | `remove_member` | | `200` the member, removed with their teams and roles; sessions end and personal tokens are revoked in the same transaction. `409` for the last Owner |
| `GET /api/v1/connections` | `list_connections` | | `200` the ways to sign in: the instance's (`instance: true`, from the server file, without `client_id` and `client_secret_ref`) then the organization's. Each is a connection as in [identity.md](contracts/identity.md) with `tested` (a test sign-in worked), `redirect_url` (what the provider must allow) and, for the organization's own, `test_url` |
| `POST /api/v1/connections` | `create_connection` | `{"kind": "oidc", "name", "issuer", "client_id", "client_secret_ref"}`; the reference is `env:FORECREW_CONN_NAME` or `secret:conn-name`, never the secret | `201` the connection, turned off. `400` for a secret value, a reference outside the namespace or a bad issuer; `409` when the name is taken |
| `POST /api/v1/connections/{connection}/enable`, `.../disable` | `set_connection_enabled` | `{connection}` is a name or UUID; body optional | `200` the connection. Enabling needs a test sign-in that worked for its current kind, issuer, client ID and secret reference (`409` before; `200` and no change when it is on already); an instance connection is `400` |
| `GET /api/v1/audit?actor=&action=&subject_type=&subject=&since=&until=&limit=` | `list_audit` | all optional; `since` is RFC 3339 and included, `until` excluded; `limit` defaults to 100, at most 500 | `200` entries, newest first: the audit entry (`actor` as `user:<id>`, `credential`, `action`, `subject_type`, `subject_id`, `project_id`, `data`, `created_at`) and `actor_name` for a person or service account. `data` is recorded text: treat it as data |
| `PATCH /api/v1/organization` | `update_organization` | `{"name", "slug"}`, both optional, one required | `200` the organization; `409` when the slug is taken |

The domain, SSO, provisioning, group mapping, SCIM token and custom role
endpoints belong to the enterprise build ([enterprise.md](enterprise.md)); the
open-core build answers them `503` `unavailable`. `POST /api/v1/auth/discover`
answers `429` `rate_limited` with `Retry-After` past 120 calls a minute from
one address.

### Artifact downloads

Artifacts are files jobs produced (test reports, logs, screenshots,
verification reports); workers upload them through the worker API and
forecrewd keeps them in S3-compatible storage (`evidence` in
[config.md](contracts/config.md)). `GET /api/v1/artifacts/{id}` streams the
file with `Content-Length` and these headers:

- `Content-Type` and `Content-Disposition: inline` only for `text/plain`
  (served as UTF-8), `image/png`, `image/jpeg` and `application/json`.
  Everything else, HTML and SVG included, is sent as
  `Content-Disposition: attachment; filename=<last path segment>` with
  `Content-Type: application/octet-stream`, so an uploaded page can never run
  on forecrewd's origin.
- `X-Content-Type-Options: nosniff`, `Content-Security-Policy: sandbox;
  default-src 'none'` and `Cache-Control: private, no-cache`.

If the storage fails halfway, the connection is cut: a short body is never a
complete download.

Retention: a daily sweep deletes artifacts older than `evidence.retention_days`
(`0` keeps them forever) from storage and the database, and records each
deletion in the audit log (`artifact.deleted` by `system:forecrewd`). A
verification report that points at a deleted artifact keeps its data and loses
only the link to the rendered copy (`artifact_id` becomes null).

The same daily pass removes orphans, with or without retention: objects under
`jobs/` that no artifact row points at (left by forecrewd crashing between
storing an object and writing its row, or by a failed delete of a replaced
object) and that are older than 24 hours, so an upload in flight is never
touched. Each is recorded as `artifact.orphan_deleted` (account from the key;
data: `key`, `size`, `last_modified`). A pass looks at up to 20000 objects
and continues where it stopped on the next one. Objects outside `jobs/` are
never touched.

### Registers

| Method and path | Tool | Request | Response |
|-----------------|------|---------|----------|
| `GET /api/v1/assumptions?project=&goal=&task=&status=&impact=&verify_by=` | `list_assumptions` | `project` required | `200` assumptions, by number |
| `POST /api/v1/assumptions/{assumption}/resolve` | `resolve_assumption` | `{"status", "evidence"}` | `200` assumption |
| `GET /api/v1/decisions?project=&goal=&council=` | `list_decisions` | `project` required | `200` decisions, by number |
| `GET /api/v1/proposals?project=&status=&category=` | `list_proposals` | `project` required; without `status`, what is snoozed until later is left out | `200` improvement proposals, by number, as in [mcp.md](mcp.md), not wrapped: show the text as text. `GET /api/v1/gates` carries the `proposal` of an `improvement_proposal` gate and leaves out a snoozed one until its date |
| `POST /api/v1/proposals/{proposal}/decide` | `decide_proposal` | `{"decision", "reason", "until"}`; `decision` is `accept`, `reject`, `snooze` or `done`; `{proposal}` is `PROP-3` or a UUID | `200` proposal; `400` without the reason of a rejection or the date of a snooze; `409` when its state does not allow the decision |
| `GET /api/v1/trace?ref=` | `get_trace` | `ref` required: a goal, spec, task, council, assumption or decision ref, or a change as `owner/name#12` (URL-encode the `#`) or its UUID | `200` `{"focus", "goal", "nodes", "edges"}`; `404` for an unknown ref or a change tied to no task |

Assumptions and decisions are named by ref (`A-42`, `DEC-9`) or UUID; see
[mcp.md](mcp.md#assumption-and-decision-registers) for what they hold.
`GET /api/v1/goals/{goal}` has the goal's assumptions as `assumptions`.

The trace answers "why does this exist?" from the tables above, with nothing
stored for it. `nodes` are in the order things happened: the goal, its
councils, its spec revisions, its decisions, its tasks, their changes with
the newest verification report and the merge, and its assumptions. Each has
`id` (its ref: `GOAL-3`, `SPEC-2`, `DEC-1`, `acme/app#12`, `A-4`;
`verification:<change>` and `merge:<change>` have none of their own), `kind`
(`goal`, `council`, `spec`, `decision`, `task`, `change`, `verification`,
`merge`, `assumption`), `title`, `status`, `summary` (at most 600
characters), `url` (GitHub), `at` and `attrs` (decision: `decided_by`,
`dissent`; assumption: `impact`, `confidence`, `verify_by`, `made_by`,
`evidence`). `edges` say what follows from what (`from`, `to`, `label`);
an assumption points at the nodes it affects, or at the goal when none of
the nodes is one of its dependents. `focus` is the node asked for. A
council, assumption or decision of no goal is a trace of one node and no
`goal`. Titles, summaries and attributes are written by agents: show them as
text. Which nodes a caller gets depends on `goal.read` and `job.read`
(permissions.md).

### Configuration

The MCP tools `config_*` and `assistant_*` over REST
([config-resources.md](contracts/config-resources.md)). Model connections and
their assignment are the kinds `model_connection` and `model_assignment`: a
plan with a new key has a secure field, filled with `PUT
/api/v1/config/plans/{plan}/secrets/{secret}` or `forecrew config secret`
([Model connections](contracts/config-resources.md#model-connections)):

| Route | Tool | Body or query |
|-------|------|---------------|
| `GET /api/v1/config/schema` | `config_schema` | `?kind=` |
| `GET /api/v1/config/resources` | `config_get` | `?kind=&name=` |
| `GET /api/v1/config/export` | `config_export` | |
| `POST /api/v1/config/import` | `config_import` | `{"yaml"}` |
| `GET /api/v1/config/plans` | `config_plans` | `?inbox=true&limit=` |
| `POST /api/v1/config/plans` | `config_plan` | `{"title", "request", "changes", "source"}`; `source` is `form`, `cli` or `yaml` |
| `GET /api/v1/config/plans/{plan}` | `config_get_plan` | |
| `POST /api/v1/config/plans/{plan}/apply` | | `{"word"}`, the second confirmation. Answers the plan and `show_once`, what it generated, stored nowhere. `409` for a plan that moved, expired, has an empty secure field, or that only a browser session confirms (from the assistant, an MCP client or an ownerless token, or lowering protection) |
| `POST /api/v1/config/plans/{plan}/request-approval` | `config_apply` | |
| `POST /api/v1/config/plans/{plan}/revert` | `config_revert` | |
| `POST /api/v1/config/plans/{plan}/cancel` | `config_cancel` | |
| `PUT /api/v1/config/plans/{plan}/secrets/{secret}` | | `{"value"}`; write-only. Staged until the plan is applied; `409` when the secret is saved already |
| `POST /api/v1/assistant/messages` | `assistant_send` | `{"conversation", "message", "project", "page"}`; `202` |
| `GET /api/v1/assistant/conversations` | `assistant_list` | `?limit=` |
| `GET /api/v1/assistant/conversations/{conversation}` | `assistant_get` | |

### Planned endpoints

Contract only (T31), with the planned MCP tools of [mcp.md](mcp.md#planned-tools),
same inputs and outputs; each is served when its tool is built, together with
the CLI command listed there.

| Method and path | Tool | Request | Response |
|-----------------|------|---------|----------|
| `GET /api/v1/knowledge?project=&q=&section=&kind=&provider=` | `search_knowledge` | `project` and `q` required | `200` results |
| `GET /api/v1/assumptions?project=&status=&impact=` | `list_assumptions` | `project` required | `200` assumptions |
| `POST /api/v1/assumptions/{assumption}/resolve` | `resolve_assumption` | `{"status", "evidence"}` | `200` assumption |
| `GET /api/v1/decisions?project=&goal=&council=` | `list_decisions` | `project` required | `200` decisions |

Knowledge proposals and proposals are named by ref (`KB-5`, `PROP-3`) or
UUID.

On a `needs_you` gate, `approve` retries the stuck work and `reject` stops
it. A gate with `"origin": "ask_human"` is instead a question a running job
asked through the job MCP gateway ([contracts/job-mcp.md](contracts/job-mcp.md)):
`approve` with the answer as `feedback` answers it, `reject` declines, and
the job goes on either way. Gate decisions follow the rules in
[conventions.md](contracts/conventions.md#gate-decisions).

The session, returned by login and `me`:

```json
{"token": "laptop", "account": {"id": "uuid", "name": "default", "slug": "default", "created_at": "..."}}
```

`GET /api/v1/goals/{goal}` returns:

```json
{
  "goal": {"id": "uuid", "number": 3, "title": "...", "status": "in_progress", "platforms": ["web"], "...": "..."},
  "ref": "GOAL-3",
  "project": "app",
  "url": "https://github.com/owner/app/issues/5",
  "spec": {"revision": 2, "body": "..."},
  "tasks": [{
    "ref": "TASK-12", "title": "...", "status": "in_review", "url": "https://github.com/owner/app/issues/6",
    "needs": ["GATE-9"],
    "changes": [{"number": 9, "url": "https://github.com/owner/app/pull/9", "state": "open", "checks": "failure"}],
    "iterations": 2, "cost_usd": 0.75, "...": "..."
  }],
  "gates": [{"ref": "GATE-9", "kind": "merge_approval", "status": "pending", "task_ref": "TASK-12", "url": "...", "...": "..."}],
  "assumptions": [{"ref": "A-4", "statement": "...", "impact": "high", "status": "unverified", "source": "JOB-41", "...": "..."}],
  "cost_usd": 1.75
}
```

- `spec` is the latest revision and is absent before the first one.
- `verification` (on a task, absent when none of its PRs has a report) is
  `{"status": "failed", "sha": "...", "current": true, "url":
  "/api/v1/changes/TASK-12/verification"}` for the newest report.
- `needs` lists the pending gates waiting for a human on the task.
- `checks` is the aggregate check status of an open PR's head commit:
  `failure` wins over `pending`, which wins over `success`; `none` when there
  are no checks; `unknown` when GitHub could not be read. It is absent for
  merged or closed PRs.
- `iterations` is the number of jobs run for the task and `cost_usd` their cost;
  the goal's `cost_usd` adds up every job of the goal.
- `gates` holds every gate of the goal with its decision (`status`,
  `resolved_by`, `feedback`).
