# Configuration

Forecrew has two config files: `forecrew.yaml` for forecrewd and `worker.yaml`
for each forecrew-worker. Both are loaded by `internal/config` with strict
decoding: unknown keys are errors. Secrets never appear in the files; they come
from environment variables and live in memory only.

Projects, MCP/API tokens and workers are not configuration: they live in the
database and are managed with the `forecrew` CLI, MCP or the web UI.

The same file shape serves self-hosted installations and the SaaS; nothing in
the code depends on which one it is.

## forecrew.yaml

```yaml
server:
  listen: ":443"                              # optional, see defaults
  public_base_url: https://forecrew.example.com
  tls_domain: forecrew.example.com            # optional
  cert_cache_dir: /var/lib/forecrew/certs     # optional

github:
  app_id: 123456
  allowlist: [octocat, hubot]

runtimes:
  claude:
    adapter: claude-code
    vendor: anthropic
    credential_env: [ANTHROPIC_API_KEY]
  codex:
    adapter: codex
    vendor: openai
    credential_env: [CODEX_API_KEY]

model_tiers:
  fast:   {anthropic: claude-haiku-4-5, openai: gpt-5-mini}
  strong: {anthropic: claude-sonnet-4-5, openai: gpt-5}
  best:   {anthropic: claude-opus-4-1, openai: gpt-5-pro}

roles:
  onboarding-analyst: {runtime: claude, tier: strong}
  product-owner:      {runtime: claude, tier: strong}
  architect:          {runtime: claude, tier: best}
  developer:          {runtime: claude, tier: strong}
  reviewer:           {runtime: codex,  tier: best}
  test-architect:       {runtime: claude, tier: best}
  environment-engineer: {runtime: claude, tier: strong}
  test-engineer:        {runtime: claude, tier: strong}
  triage:               {runtime: claude, tier: fast}

policy:
  allow_same_vendor_review: false

pools:                                        # optional; these are the defaults
  build:
    roles: ["*"]
    kinds: [agent, command]
    credentials: {runtimes: ["*"], repo_token: write}
  control:
    roles: ["*"]
    kinds: [agent]
    credentials: {runtimes: ["*"], repo_token: read}
  actions:
    roles: []
    kinds: [command]
    credentials: {runtimes: [], repo_token: read}

evidence:                                     # optional; artifact storage
  endpoint: http://garage:3900
  bucket: forecrew-evidence
  region: garage
  path_style: true
  max_artifact_size: 104857600
  retention_days: 90

sizing:                                       # optional; these are the defaults
  product:
    impact: [high]
    reversibility: [hard, irreversible]
  technical:
    impact: [medium, high]
    components: 2
```

Model names above are examples; use the ones your vendors offer.

### Fields

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `server.listen` | string | no | `:443` if `tls_domain` is set, else `:8080` | Address forecrewd listens on. |
| `server.public_base_url` | URL | yes | | Public URL of the installation (`https://...`; `http://` only for local dev). Trailing `/` is removed. Workers, GitHub webhooks and MCP clients use it. |
| `server.tls_domain` | string | no | empty | When set, forecrewd serves HTTPS itself with automatic Let's Encrypt certificates (TLS-ALPN-01 on the listen port) for this domain, and `public_base_url` must be `https://<tls_domain>`. When empty, forecrewd serves plain HTTP for a load balancer that terminates TLS, or for local dev. |
| `server.cert_cache_dir` | path | no | `/var/lib/forecrew/certs` when `tls_domain` is set | Where certificates are cached. Must be persistent. |
| `github.app_id` | integer | yes | | GitHub App ID. |
| `github.allowlist` | list of strings | no | empty | GitHub logins allowed to command `@forecrew` in issues and PRs. Compared case-insensitively. Empty means nobody. |
| `runtimes` | map name → runtime | yes, ≥1 | | Agent runtimes. Names match `^[a-z0-9][a-z0-9-]*$` and are referenced by `roles`. |
| `runtimes.<name>.adapter` | `claude-code` \| `codex` | yes | | Runtime adapter. Workers advertise `runtime:<adapter>` and jobs require it. |
| `runtimes.<name>.vendor` | string | yes | | Model vendor; selects the model in `model_tiers`. |
| `runtimes.<name>.credential_env` | list of strings | no | empty | Names of env vars forecrewd reads at startup and passes to the worker in the job spec `env`. Each must be set. Empty means the key must be in the worker's own environment (`ANTHROPIC_API_KEY`, `CODEX_API_KEY`). A login on the worker machine is not used by either runtime: every run gets an isolated config dir (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`). A Codex job with no `CODEX_API_KEY` from either place fails with an error naming it. |
| `model_tiers` | map tier → vendor → model | yes for every (tier, vendor) a role uses | | Tiers are `fast`, `strong`, `best`. |
| `roles` | map role → `{runtime, tier}` | no | empty | Runtime and tier per role. Role definitions (prompts, write paths, schemas) live in `packs/roles`. |
| `policy.allow_same_vendor_review` | bool | no | `false` | When false, `reviewer` and `developer` must use runtimes of different vendors. |
| `pools` | map name → pool | no | `build`, `control`, `actions` as above | Worker pools (`config.Pool`). Names are `domain.ValidName`s. An entry replaces the default of the same name; the defaults not redefined always exist. Every worker belongs to one pool (worker-api.md, Pools). |
| `pools.<name>.roles` | list of strings | no | empty | Roles whose agent jobs the pool runs, or `"*"` for all. Not used for command jobs. |
| `pools.<name>.kinds` | list of `agent` \| `command` | yes | | Job kinds the pool runs. |
| `pools.<name>.credentials.runtimes` | list of strings | no | empty | Runtimes (keys of `runtimes`) whose `credential_env` the pool's jobs may receive, or `"*"`. A job whose runtime is not listed is leased without `env`. |
| `pools.<name>.credentials.repo_token` | `write` \| `read` \| `none` | yes | | Widest repo token scope the pool's jobs receive. A writing role's job is refused in a pool without `write`. |
| `max_test_results_size` | integer bytes | no | `33554432` (32 MiB) | Largest request body of `complete` (worker-api.md), which carries the test results of a command job; larger bodies get `413`. Every other worker API request is limited to 1 MiB. |
| `evidence` | object | no | absent | S3-compatible object storage for artifacts (test reports, logs, screenshots, verification reports). Absent: artifact uploads answer `503` (worker-api.md). |
| `evidence.endpoint` | URL | yes | | S3 endpoint, `https://` (or `http://` on a private network such as Docker Compose). |
| `evidence.bucket` | string | yes | | Bucket; must exist. |
| `evidence.region` | string | no | `us-east-1` | Region sent with requests. |
| `evidence.path_style` | bool | no | `false` | Path-style URLs (`<endpoint>/<bucket>/<key>`), needed by most self-hosted servers. |
| `evidence.max_artifact_size` | integer bytes | no | `104857600` (100 MiB) | Largest artifact a worker may upload; larger uploads get `413`. |
| `evidence.retention_days` | integer ≥ 0 | no | `0` | Artifacts older than this are deleted; `0` keeps them forever. |
| `sizing.product.impact` | list of `low` \| `medium` \| `high` | no | `[high]` | Impacts that give a goal the product council (goal.size, playbooks.md). An empty list means none. |
| `sizing.product.reversibility` | list of `easy` \| `hard` \| `irreversible` | no | `[hard, irreversible]` | Reversibilities that give a goal the product council. |
| `sizing.technical.impact` | list of `low` \| `medium` \| `high` | no | `[medium, high]` | Impacts that give a goal the technical council. |
| `sizing.technical.components` | integer ≥ 1 | no | `2` | Distinct areas (paths or components) the triage names that give a goal the technical council; `0` or absent keeps the default. |

A job may run in a pool when the pool lists its kind and, for an agent job,
its role (or `"*"`), and when the role writes, the pool's `repo_token` is
`write` (`config.Pool.Allows`). A job's pool comes from its role pack's
`pool` (roles.md, default `build`) or, for a command job, from its
environment (`actions` for provider `actions`, otherwise `build`,
`domain.CommandPool`); a job its pool does not allow is refused when it is
created. Pool `actions` has no worker daemons: forecrewd runs its jobs on
GitHub-hosted runners.

### Sizing

A goal's triage (the `triage` role, playbooks.md) grades its impact and
reversibility and names the areas it touches. `goal.size` applies this rule:
a goal gets the product council when its impact is one of
`sizing.product.impact` or its reversibility is one of
`sizing.product.reversibility`; it gets the technical council when its impact
is one of `sizing.technical.impact` or its triage names at least
`sizing.technical.components` distinct areas. Small reversible goals therefore
get no council. A goal created with councils (create_goal) is not triaged.

### Environment variables

| Variable | Required | Meaning |
|----------|----------|---------|
| `FORECREW_DATABASE_URL` | yes | Postgres URL, e.g. `postgres://forecrew:...@db:5432/forecrew?sslmode=require`. |
| `FORECREW_GITHUB_PRIVATE_KEY` | yes | GitHub App private key, PEM. |
| `FORECREW_GITHUB_WEBHOOK_SECRET` | yes | GitHub App webhook secret. |
| each `runtimes.*.credential_env` entry | yes | Runtime credentials, e.g. `ANTHROPIC_API_KEY`, `CODEX_API_KEY`. |
| `FORECREW_EVIDENCE_ACCESS_KEY_ID` | when `evidence` is set | Access key of the evidence bucket. |
| `FORECREW_EVIDENCE_SECRET_ACCESS_KEY` | when `evidence` is set | Secret key of the evidence bucket. Never logged. |

forecrewd takes the config path from `--config` (default `forecrew.yaml`).

### Validation

`config.Parse` reports every problem at once, wrapping `config.ErrInvalid`:
missing or malformed `public_base_url`, `public_base_url` not matching
`tls_domain`, missing env secrets, `app_id` ≤ 0, no runtimes, unknown adapter,
missing vendor, invalid runtime name, unknown tier, role referring to an
unknown runtime, role tier without a model for the runtime's vendor,
reviewer/developer on the same vendor without `allow_same_vendor_review`,
invalid pool names, pools without kinds or with unknown kinds, empty role
names, pool runtimes that are not configured, unknown `repo_token` scopes,
and, when `evidence` is set, a malformed endpoint, no bucket, a negative
size or retention, and missing evidence keys.

## worker.yaml

```yaml
server_url: https://forecrew.example.com
name: build-box-1
labels: [go, node, docker]
work_dir: /var/lib/forecrew-worker
max_jobs: 2
```

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `server_url` | URL | yes | | forecrewd's `public_base_url`. The worker only dials out to it. |
| `name` | string | no | host name | Display name; the server identifies the worker by its token. |
| `labels` | list of strings | no | empty | Toolchains and anything else jobs may `need`. Compared case-insensitively. |
| `work_dir` | path | no | `<user cache dir>/forecrew-worker` | Where per-job workspaces are created and removed. |
| `max_jobs` | integer ≥ 1 | no | `1` | Jobs run at once. |

On top of `labels` the worker adds, when polling:

- its OS label: `linux`, `darwin` or `windows` (Go's `GOOS`; macOS also gets `macos`);
- `runtime:<adapter>` for each runtime CLI it finds (`runtime:claude-code`,
  `runtime:codex`).

| Variable | Required | Meaning |
|----------|----------|---------|
| `FORECREW_WORKER_TOKEN` | yes | Worker token printed once by `forecrew worker add`. |

forecrew-worker takes the config path from `run --config` (default `worker.yaml`).
