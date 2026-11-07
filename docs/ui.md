# Web UI

A single-page app in [`web/`](../web), served by forecrewd at `/`. It is a thin
client over [`/api/v1`](api.md): it shows what the API returns and sends the
decisions you make, and holds no business logic. GitHub stays the place to
review code; the UI links to it.

Screens: Home (what needs me, is everything healthy), Inbox (decide gates,
filtered by project; a goal page decides its own pending gates with the same
cards), Goals, Councils, Decisions, Projects, Jobs, Quality, Workers, Settings and Your account (dig into detail; a queued or
running job can be canceled from the job page and from the job tables),
Assumptions, Knowledge and the trace ("why does this exist?"), and a
command menu (Ctrl+K or Cmd+K) to jump anywhere or start a goal, a council or
an onboarding.

## Organizations and access

The screens follow `2 - UI design/Configuration and fleet UI.md`.

- **Sign-in** (`/login`). An email field first (`POST /auth/discover` with the
  email: a company whose domain requires its own sign-in leads straight to it),
  then one button per way to sign in: the server's, and those of the
  organization of `?org=` or of the invitation. `?redirect=` is carried through
  the sign-in (paths of this site only). The pasted token shows only where the
  server sets `auth.token_login`. Refusals (not invited, the domain requires
  SSO, provider error) are the plain pages of the server.
- **Permission-aware.** Everything is driven by `/me` (`permissions` on the
  organization, `projects` for project roles). A section or action the caller
  cannot even read is left out; one they can read but not use is disabled and
  names the permission it needs (`PermButton`). Decisions the caller may not
  resolve (`can_resolve`) show no buttons and ignore the keyboard shortcuts.
- **Settings** (`/settings`, a section only for the permission that reads
  it): General (name and slug, `org.manage` changes them), People (tabs
  Members, Teams, Roles, Service accounts; `org.read`), Sign-in
  (`connection.manage`) and Change history (the audit log, `audit.read`, with
  filters by person, action, what it was done to and dates; what was recorded
  is shown as text).
  - Members: role, direct access and teams ("direct", "through team Mobile"),
    status. Change role (the new role is given before the old is taken),
    suspend (after confirming), resume and remove need `member.manage`; the
    last Owner's actions are disabled and say why, and so are all actions on a
    member who holds roles the caller does not fully hold (an Admin and an
    Owner), naming the permission missing;
    members provisioned by SCIM show "Managed by your identity provider" and
    no controls. Invitations (by email or GitHub login, roles, expiry, revoke):
    the link is shown once with Copy.
  - Teams: members, and the roles each team holds on the organization and on
    projects. Roles lists the built-in roles read-only as capabilities by area.
  - Sign-in: the server's connections are read-only ("Managed by server file").
    Adding a company sign-in takes a client ID and the client secret in a
    secure field (a password input that goes through a change plan to the secret
    store, `store:conn-<name>`, and is never shown again), starts turned off, shows the redirect address to allow at the
    provider, and can be turned on only after Test sign-in worked.
- **Your account** (`/account`; `/tokens` leads here): linked sign-ins,
  personal tokens (create with name, scope and expiry, shown once with Copy;
  scopes and expiry shown when the server sends them; revoke) and browser
  sessions (end one, or sign out everywhere). Connected apps wait for the
  OAuth screen (T55).
- **Project access** (a section of the project page): who has which role,
  directly or through a team; Owners and Admins show "through organization
  role", read-only. Adding and removing needs `project.manage` on the project;
  choosing people needs `org.read`.
- **Allow the command line** (`/cli-login`): where `forecrew login` sends the
  browser; the person confirms, the page asks the server for a one-time code
  and sends the browser to the CLI's loopback address (the code is also shown
  for a machine without a browser).

## Look

The UI is stock shadcn/ui, as on ui.shadcn.com: style `base-nova` (Base UI
primitives), base color neutral, the stock light and dark theme variables,
radius `0.625rem` and the Geist font. This is the owner's decision and replaces
the visual language of the original T18 brief (its palette, Inter and JetBrains
Mono, radius scale and 44 px touch targets). Controls use stock heights.
`font-mono` is Tailwind's system monospace stack, used for logs, refs and code.

Layout follows the design screens in `2 - UI design/Screens` (structure and
placement only; colors and fonts are stock). What the MVP has no data for is
left out, not faked:

- **Shell.** A left sidebar with the app name, the organization (a switcher
  when `/me` lists two or more: it moves the session with
  `PUT /session/organization`; a session from an organization's own connection
  lists only that one), a project switcher, the nav, and at the bottom Settings
  (only when the caller may read a section) and the account menu: Your account,
  Sign out and Sign out everywhere. The switcher stays on the current page and sets the `project` filter in
  the address; Inbox, Goals and Jobs filter by it and their sidebar links keep
  it. It is the only project filter (the lists have no project select of their
  own). On a phone the sidebar becomes a bottom bar (Home, Inbox, Jobs, Menu).
- **Home.** Command bar (opens the command menu) with a New goal button, the
  date and how many decisions wait, three tiles (decisions waiting, agents
  working, workers online), Needs you with Review or Decide (they open the
  Inbox on that decision), Active goals with a stage bar, and Live activity
  and Fleet in a right column.
- **Inbox.** A list with filter chips and the keyboard hint on the left, the
  selected decision on the right: its actions first (Open on GitHub at the
  right end), then the summary, then, on a merge approval, the pull request's
  verification (status, blocking reasons, environment tiles with tool versions and
  the cells with no result, the
  criteria traceability table, flaky tests and the link to the rendered report),
  then the feedback box. A pull request with no report yet says "No verification report yet". The
  goal page lists the verification status of each task (marked stale when the
  report is of an earlier commit) and opens its report on demand.
- **Inbox cards by kind.** A `council_decision` card shows the council's brief
  (read with `get_council`): each option with its pros, cons, cost, risk and
  votes, the dissent, the assumptions and the open questions. Approve accepts
  the recommendation; Request changes sends the feedback as the constraint of
  one more round (not offered once the council had its human round); Reject
  fails the council. An `assumption_review` card shows the statement, impact,
  confidence, source and dependents of the assumption (read from the register
  by the `Review A-n:` title): Accept approves the gate, Invalidate rejects it,
  Verify resolves the assumption with evidence through `resolve_assumption`,
  which closes the gate. A question a job asked with `ask_human` (a `needs_you`
  gate with `origin: ask_human`) has its own chip and words, apart from
  escalations: the answer is required and goes as the approval's feedback;
  Decline rejects it. The other new kinds (knowledge, access, improvement) get
  a label and a chip and the generic card. J and K move, A approves and R
  requests changes, on every card.
- **Councils.** The list shows every council of the chosen project (of all
  projects without one), newest first, with a status filter. A council page has
  the question and constraints (a form adds one while it runs), the members
  (named `role@runtime`, known from what they contributed), the proposals,
  critiques and revisions side by side with the check of their cited sources,
  and the brief. The devil's advocate's dissent, which its seat requires, is
  shown apart from the dissent of members who disagreed. Everything agents
  wrote is rendered as plain text; the raw output of a contribution is one
  click away. A council that waits for a human links to its card in the Inbox.
  Start a council from the Councils page, the command menu or a goal page.
- **Decisions.** The decisions register of the chosen project, newest first:
  the question, the choice among the options, who decided (a person, or the
  policy), the reasons, dissent, the assumptions it rests on, and a link to
  its council, and for a decision that belongs to a goal a link to the
  Gates section of the goal, where the decided gate is listed.
- **Quality.** Per project (all of them, or the one the switcher picks): the
  latest run outside a pull request (the baseline) and each open pull request,
  from `GET /api/v1/quality`. A run records no branch, so the baseline is the
  newest commit tested outside a pull request, not necessarily one on the
  default branch; heads of open pull requests and base checks are left out. Each
  has the totals (passed, failed, skipped, and flaky where a report says), a
  suites by environments grid, the failing tests, and for a pull request its
  verification report (criteria coverage, flaky tests). A result in the grid is
  a link to its job and says its status and counts in words, so colour is never
  the only signal; an empty environment is the worker's host, and a cell that
  the report of the current commit waits for and has no result for reads
  "required, no result" in the grid and in the environment tiles. Live updates,
  the trace view and coverage change are not part of it.
- **Trace** (`/trace/:ref`). The chain behind a goal, task, spec, council,
  decision, change or assumption, from `GET /api/v1/trace`: a vertical list in
  the order things happened (goal, councils, spec revisions, decisions, tasks,
  pull requests, verification, merge) with each step's origin, the assumptions
  under the steps they affect, and the selected step's detail beside it. An
  assumption is verified, accepted or invalidated in place, with its evidence.
  Goal, task, gate and job pages link it ("Why?"); a gate or job links the
  trace of its task, else of its goal.
- **Assumptions.** The register with filters for status and impact (and the
  sidebar's project; without one every project's register is listed), each item
  with what rests on it, its evidence and the same inline resolution.
- **Knowledge.** For the sidebar's project (the only project, when there is
  one): entries (sections with provider, location, owner and whether protected;
  search with kind and provider filters; an entry with its provenance, last
  verified date, a "needs re-verify" badge and the link to its source),
  proposals (pull requests or the gate to decide, and a form that calls
  `propose_knowledge`), agent notes (the project's, or the project's and one
  goal's, which can be deleted after a confirmation) and the integrations of
  the approved profile with their status. A search hit shows its excerpt; the
  listing has whole entries. The "save as lesson" box on gate feedback waits for
  the lessons flag of T39.
- **Job.** A back link, the title with the state badge and a line with goal,
  worker and model, the log beside the details, the cost and the tokens; Cancel
  stays at the top right. A command job shows its test counts and has no
  runtime or model; the Artifacts list (name, size, type, download link) shows
  for a command job always and for an agent job only when it has files.

- `web/src/index.css` holds the theme exactly as `shadcn init` writes it, with
  one change: dark mode follows `prefers-color-scheme` instead of a `.dark`
  class. It adds two things stock lacks: `--success` and `--warning` for
  `StatusBadge` (errors use `--destructive`). They always sit next to words,
  and pass WCAG AA contrast in light and dark.
- `web/components.json` is the stock `init` output. Do not edit
  `src/components/ui` by hand; regenerate it with the CLI.
- Dialogs lock the page's scroll through Base UI, which sets styles from
  script; the Content-Security-Policy needs no inline `<style>` for it.

## Run it locally

You need Node 24 or newer and pnpm 11.

```sh
make web-dev            # Vite dev server on http://localhost:5173
```

The dev server proxies `/api` and `/auth` to a forecrewd on `http://localhost:8080` (set
`FORECREW_URL` for another address). Start that forecrewd on your machine as
usual and sign in through a connection, or set `auth.token_login` and log in with
a token (`forecrew token create dev --user <you>`).

The session cookie is `Secure`, so the browser only keeps it on HTTPS or on
`localhost`. Run the UI on `localhost` as above; opening the dev server through
another host name over plain HTTP logs you out on every request.

`make web` builds the UI into `internal/webui/dist`, which `forecrewd` embeds
(`go build` then includes it). Without a build the Go code still compiles; `/`
answers "UI not built: run make web". Only `dist/.gitkeep` is committed.

## Checks

| Command | What |
|---------|------|
| `make web-lint` | ESLint and `tsc --noEmit` (strict) |
| `make web-test` | Vitest and Testing Library, API answered by MSW |
| `make web-cover` | The same with coverage; fails under 80% of lines in `web/src` (shadcn's `components/ui` is excluded) |
| `make e2e` | Playwright on the production build (`vite preview`), with axe accessibility checks at desktop and phone widths |

`make lint`, `make test` and `make cover` include the first three. CI runs them
in the `web` job, with the end-to-end tests.

To run the end-to-end tests with a Chromium you already have, set
`PLAYWRIGHT_CHROMIUM_PATH` to its executable; otherwise `make e2e` runs
`playwright install chromium`.

## Architecture

```
web/src
  api/          types.ts (written from docs/api.md), client.ts (the only place that calls fetch), queries.ts (TanStack Query hooks)
  components/   shared components; components/ui is shadcn's (added with its CLI, not edited; see Look)
  routes/       one file per page (goals.tsx has the list and the New goal form; knowledge.tsx has the entries and knowledge-tabs.tsx the other views)
  lib/          formatting and small helpers
  router.tsx    TanStack Router tree; the login redirect and the 401 handling
  test/         MSW server, render helper, fixtures/
```

- **Login.** The login page links to the connections of
  `POST /api/v1/auth/discover`, passing on `org` and `invite` from its URL. With
  `auth.token_login` on it also posts a pasted token to `POST /api/v1/session`.
  Either way the server answers with an HttpOnly cookie, and nothing is kept in
  the page or in browser storage. Every `401` goes back to the login page, which
  returns to where you were. State-changing calls are sent as
  `application/json`, which the API requires for cookie sessions.
- **Cancel and revoke.** "Cancel job" (job page, jobs page, goal jobs table) and
  "Revoke" (Your account) always ask in a dialog first. A running job stops on
  its worker within about 30 s and its playbook takes its failure path; the
  job, job lists and gates refresh after it. Your account lists your tokens
  (the one you are using is badged "this session"); revoking that one logs you
  out and goes to the login page. Tokens are created there too, or with the CLI.
- **Live data.** Lists poll every 5 s with TanStack Query; a running job's log
  polls every 2 s and asks only for events after the last one it has
  (`after_seq`). Polling pauses while the tab is hidden.
- **Untrusted text.** Agent output, GitHub text and summaries are rendered as
  text or as markdown without raw HTML (`react-markdown`, no HTML plugin,
  images dropped). ESLint forbids `dangerouslySetInnerHTML` and `innerHTML`.
  forecrewd sends `Content-Security-Policy: default-src 'self'`, so the UI loads
  only its own files (no inline scripts, no data: URIs; fonts are bundled).
- **Goal stage.** The goal list has no finer state than the goal status, so the
  stage is derived from it: draft and spec_review are "spec", planned is
  "planning", in_progress is "building".
- **Serving.** `internal/webui` embeds `dist`, serves files that exist, falls
  back to `index.html` for client-side routes (not for `/api`, `/mcp`,
  `/webhooks`, `/worker` or `/healthz`), caches `assets/` as immutable and
  `index.html` as no-cache.

## Add a page

1. Add `web/src/routes/<page>.tsx` exporting the component. Use the hooks in
   `api/queries.ts` (add one there, and the call to `api/client.ts` with its
   types in `api/types.ts`, if the endpoint is new) and wrap queries in
   `<Async>` for the loading, error and empty states. Put anything a second
   page also needs in `components/`.
2. Register it in `router.tsx` under the `app` route (pages that need a
   session) and add a link in `components/AppShell.tsx` or the command menu.
3. Write `<page>.test.tsx` with `renderApp("/path")`: the loading, empty and
   error states, and what the page does. Add the page to the list in
   `e2e/a11y.spec.ts`.
4. Add a UI component with `pnpm dlx shadcn@4.23.0 add <name>` from `web/`
   rather than writing one by hand.

If a screen needs data the API does not give, add the endpoint to `/api/v1`
first (it is one more MCP tool); the UI does not compute it.

## API fixtures

Web tests never hand-write API answers. `internal/api/fixtures_test.go` calls
the real handlers with the testkit fakes and writes one JSON response per
endpoint (and a few errors and empty lists) to `web/src/test/fixtures/`:

```sh
go test ./internal/api -update     # rewrite the fixtures
go test ./internal/api             # fails when a response no longer matches
```

Change an API field, run the update, and `git diff web/src/test/fixtures`
shows what the UI must follow. Vitest serves the files with MSW
(`src/test/server.ts`) and Playwright with `page.route()` (`e2e/api.ts`); a
field the UI reads but the API dropped fails the Go test first, then the types
(`tsc`) and the tests. The fixtures use fixed IDs and times, and the web tests
freeze the clock at the time they were recorded (2026-03-01 12:00 UTC).
