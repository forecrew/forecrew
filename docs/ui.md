# Web UI

A single-page app in [`web/`](../web), served by forecrewd at `/`. It is a thin
client over [`/api/v1`](api.md): it shows what the API returns and sends the
decisions you make, and holds no business logic. GitHub stays the place to
review code; the UI links to it.

Screens: Home (what needs me, is everything healthy), Inbox (decide gates,
filtered by project; a goal page decides its own pending gates with the same
cards), Goals, Projects, Jobs, Workers and Tokens (dig into detail; a queued or
running job can be canceled from the job page and from the job tables), and a
command menu (Ctrl+K or Cmd+K) to jump anywhere or start a goal or an
onboarding.

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

- **Shell.** A left sidebar with the app name, a project switcher, the nav, and
  Tokens, who is signed in and Log out at the bottom (there is no Settings page
  yet). The switcher stays on the current page and sets the `project` filter in
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
  right end), then the summary, then the feedback box. Verification, reviews
  and assumptions are not shown (no data).
- **Job.** A back link, the title with the state badge and a line with goal,
  worker and model, the log beside the details, the cost and the tokens; Cancel
  stays at the top right.

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

The dev server proxies `/api` to a forecrewd on `http://localhost:8080` (set
`FORECREW_URL` for another address). Start that forecrewd on your machine as
usual, create a token (`forecrew token create dev`) and log in with it.

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
  routes/       one file per page (goals.tsx has the list and the New goal form)
  lib/          formatting and small helpers
  router.tsx    TanStack Router tree; the login redirect and the 401 handling
  test/         MSW server, render helper, fixtures/
```

- **Login.** The login page posts the pasted token to `POST /api/v1/session`;
  the server answers with an HttpOnly cookie. The token is never kept in the
  page or in browser storage. Every `401` goes back to the login page, which
  returns to where you were. State-changing calls are sent as
  `application/json`, which the API requires for cookie sessions.
- **Cancel and revoke.** "Cancel job" (job page, jobs page, goal jobs table) and
  "Revoke" (Tokens page) always ask in a dialog first. A running job stops on
  its worker within about 30 s and its playbook takes its failure path; the
  job, job lists and gates refresh after it. The Tokens page lists the account's
  tokens (the one you are using is badged "this session"); revoking that one
  logs you out and goes to the login page. Tokens are only created on the server
  (`forecrew token create`), never in the UI.
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
