# ADR 0001: MVP decisions

Status: accepted

## Context

Forecrew lets role-based AI agents build software from a human's goals. An
orchestrator hands agent jobs to workers; humans steer through a web UI, MCP
and GitHub. The MVP must be small enough to build in parallel tasks, and must
not paint us into a corner for the hosted product.

## Decisions

1. **One codebase, shipped two ways.** The same binaries run self-hosted by
   anyone and as our SaaS. Nothing in code is specific to either; the
   differences live in config.

2. **Accounts from day one, one account in the MVP.** Every project belongs
   to an account, created at install. Every project-scoped table carries
   `account_id` and every store call takes it, so multi-tenant SaaS needs no
   data migration. No signup, billing or per-account auth in the MVP.

3. **Go.** Three binaries from one module: `forecrewd` (orchestrator),
   `forecrew-worker` (worker daemon for Linux, macOS and Windows) and
   `forecrew` (admin CLI, Cobra). Standard library first. React for the web UI.

4. **Postgres is the only stateful dependency.** State, the work queue and an
   append-only audit log live in Postgres. Jobs are leased with
   `SELECT ... FOR UPDATE SKIP LOCKED`, 5-minute leases renewed by
   heartbeats; expired leases are requeued by a reaper. No separate broker.

5. **Our own pull-based worker daemon.** Workers run on any machine and only
   dial out to forecrewd over HTTPS; forecrewd never connects to a worker.
   A worker long-polls with its labels (os, toolchains, `runtime:<adapter>`)
   and gets the oldest queued job whose needs it satisfies. Each job runs in
   a fresh workspace and pushes only `agent/*` branches. External execution
   providers (cloud machines started on demand) come later and will only
   start machines running the same daemon.

6. **GitHub App.** Forecrew acts on repos through a GitHub App: webhooks in,
   issues, PRs, labels and checks out. Workers get a short-lived installation
   token scoped to the one repo of the job, with contents write only for
   writing roles. The orchestrator, not the worker, opens and updates PRs.

7. **Per-worker tokens.** `forecrew worker add` prints a token once; only its
   hash is stored and it can be revoked. The token identifies worker and
   account.

8. **Claude Code and Codex as runtimes.** Both run headless behind one
   adapter interface (`runtime.Adapter`). Roles are YAML config choosing a
   runtime and a model tier (fast, strong, best). By default the reviewer
   must use a different vendor than the developer.

9. **Public HTTPS without a reverse proxy.** Each installation is reached on
   its own domain. forecrewd either serves HTTPS itself with automatic
   Let's Encrypt certificates (`server.tls_domain`) or plain HTTP behind the
   operator's load balancer that terminates TLS.

10. **Humans merge.** Agents never merge on their own. A change is merged by
    a human on GitHub, or by forecrewd when a human approves the
    merge_approval gate (web UI, MCP or `@forecrew` from an allowlisted
    login).

## Consequences

- Contracts in `internal/domain` and `docs/contracts` are fixed first so the
  following tasks can work in parallel.
- Running Forecrew needs Postgres, a GitHub App and at least one worker
  machine with a runtime CLI.
- Moving to multi-tenant SaaS needs signup, billing and per-account auth, but
  no schema change.
