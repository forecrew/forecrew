# Configuration

Forecrew has two config files: `forecrew.yaml` for forecrewd and `worker.yaml`
for each forecrew-worker. Both are loaded by `internal/config` with strict
decoding: unknown keys are errors. Secrets never appear in the files; they come
from environment variables and live in memory only.

Projects, organizations, people, roles, MCP/API tokens and workers are not
configuration: they live in the database and are managed with the
`forecrew` CLI, MCP or the web UI. Only instance-level sign-in connections
and instance admins are configured here (`auth`, identity.md).

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
  accept_recommendations: []                  # council kinds; see below

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

testing:                                      # optional; default: no project
  projects: []                                # e.g. [app]: its goals run goal-v2, with the testing system

auth:                                         # optional; these are the defaults
  session_idle: 168h
  session_absolute: 720h
  token_default_ttl: 2160h
  token_login: false
  instance_admins: [ana@example.com, 'github:583231']   # example; default empty
  connections:                                # example; default none
    github:
      kind: github
      client_id: Iv1.0123456789abcdef
      client_secret_ref: env:FORECREW_GITHUB_CLIENT_SECRET
    google:
      kind: google
      client_id: 1234-abc.apps.googleusercontent.com
      client_secret_ref: secret:forecrew-google
```

Model names above are examples; use the ones your vendors offer.

### Other models: opencode

The `opencode` adapter reaches every other provider, so any role can run on
any model. Its models are in `provider/model` form, and the vendor of the
runtime keys `model_tiers` as usual. A DeepSeek runtime:

```yaml
runtimes:
  deepseek:
    adapter: opencode
    vendor: deepseek
    credential_env: [DEEPSEEK_API_KEY]

model_tiers:
  fast:   {deepseek: deepseek/deepseek-chat}
  strong: {deepseek: deepseek/deepseek-chat}
  best:   {deepseek: deepseek/deepseek-reasoner}

roles:
  developer: {runtime: deepseek, tier: strong}
```

A local Ollama, or anything else that speaks the OpenAI API, is a `base_url`.
The part of the model before the `/` names the provider and is free to choose;
the rest is the model the endpoint knows. Ollama takes no key:

```yaml
runtimes:
  local:
    adapter: opencode
    vendor: ollama
    base_url: http://localhost:11434/v1

model_tiers:
  strong: {ollama: ollama/qwen3-coder}

roles:
  reviewer: {runtime: local, tier: strong}
```

The address is seen from the worker, so `localhost` is the worker machine. A
custom endpoint receives the repo code in the prompts and the runtime's key.

### Fields

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `server.listen` | string | no | `:443` if `tls_domain` is set, else `:8080` | Address forecrewd listens on. |
| `server.public_base_url` | URL | yes | | Public URL of the installation (`https://...`; `http://` only for local dev). Trailing `/` is removed. Workers, GitHub webhooks and MCP clients use it. |
| `server.tls_domain` | string | no | empty | When set, forecrewd serves HTTPS itself with automatic Let's Encrypt certificates (TLS-ALPN-01 on the listen port) for this domain, and `public_base_url` must be `https://<tls_domain>`. When empty, forecrewd serves plain HTTP for a load balancer that terminates TLS, or for local dev. |
| `server.cert_cache_dir` | path | no | `/var/lib/forecrew/certs` when `tls_domain` is set | Where certificates are cached. Must be persistent. |
| `github.app_id` | integer | yes | | GitHub App ID. |
| `github.allowlist` | list of strings | no | empty | GitHub logins allowed to command `@forecrew` in issues and PRs of an organization where nobody holds a role yet. Compared case-insensitively. Empty means nobody. Deprecated: from an organization's first role binding on it is ignored there, and a command needs a linked GitHub identity with the permission ([permissions.md](permissions.md#what-every-entry-point-needs)). |
| `runtimes` | map name → runtime | yes, ≥1 | | Agent runtimes. Names match `^[a-z0-9][a-z0-9-]*$` and are referenced by `roles`. |
| `runtimes.<name>.adapter` | `claude-code` \| `codex` \| `opencode` | yes | | Runtime adapter. Workers advertise `runtime:<adapter>` and jobs require it. |
| `runtimes.<name>.vendor` | string | yes | | Model vendor; selects the model in `model_tiers`. |
| `runtimes.<name>.credential_env` | list of strings | no | empty | Names of env vars forecrewd reads at startup and passes to the worker in the job spec `env`. Each must be set. Empty means the key must be in the worker's own environment (`ANTHROPIC_API_KEY`, `CODEX_API_KEY`, the provider's variable for opencode). A login on the worker machine is not used by any runtime: every run gets an isolated config dir (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, opencode's XDG directories). A Codex job with no `CODEX_API_KEY` from either place fails with an error naming it. |
| `runtimes.<name>.base_url` | URL | no | empty | Custom model endpoint, sent with the job when it is leased. Must be `https://`, or `http://` only to `localhost` or a loopback or private IP address; a host name is not enough for `http`, because it could resolve anywhere. No credentials, query, fragment or braces. `claude-code` gets it as `ANTHROPIC_BASE_URL`, `codex` as `openai_base_url`, and `opencode` as a custom OpenAI-compatible provider named by the model's provider part. The first `credential_env` holds the endpoint's key (opencode; the others read their usual variable). A job with that variable set nowhere fails with an error naming it. Empty means the vendor's own. |
| `model_tiers` | map tier → vendor → model | yes for every (tier, vendor) a role uses | | Tiers are `fast`, `strong`, `best`. A vendor of an `opencode` runtime has its models in `provider/model` form. |
| `roles` | map role → `{runtime, tier}` | no | empty | Runtime and tier per role. Role definitions (prompts, write paths, schemas) live in `packs/roles`. |
| `policy.allow_same_vendor_review` | bool | no | `false` | When false, `reviewer` and `developer` must use runtimes of different vendors. |
| `policy.accept_recommendations` | list of council kinds | no | empty | Council kinds (`domain.CouncilKind`) whose recommendation is accepted without a human; the decision is recorded as `policy`. Empty means every brief opens a `council_decision` gate. `reversible` is declared by the facilitator, an agent, and nothing verifies it, so this policy rests on that claim. A brief is accepted only when it says `reversible` and no member other than the devil's advocate, whose dissent its seat requires, dissents or votes against the recommendation. |
| `pools` | map name → pool | no | `build`, `control`, `actions` as above | Worker pools (`config.Pool`). Names are `domain.ValidName`s. An entry replaces the default of the same name; the defaults not redefined always exist. Every worker belongs to one pool (worker-api.md, Pools). |
| `pools.<name>.roles` | list of strings | no | empty | Roles whose agent jobs the pool runs, or `"*"` for all. Not used for command jobs. |
| `pools.<name>.kinds` | list of `agent` \| `command` | yes | | Job kinds the pool runs. |
| `pools.<name>.credentials.runtimes` | list of strings | no | empty | Runtimes (keys of `runtimes`) whose `credential_env` the pool's jobs may receive, or `"*"`. A job whose runtime is not listed is leased without `env`. |
| `pools.<name>.credentials.repo_token` | `write` \| `read` \| `none` | yes | | Widest repo token scope the pool's jobs receive. A writing role's job is refused in a pool without `write`. |
| `max_test_results_size` | integer bytes | no | `33554432` (32 MiB) | Largest request body of `complete` (worker-api.md), which carries the test results of a command job; larger bodies get `413`. Every other worker API request is limited to 1 MiB. |
| `actions.max_in_flight` | integer ≥ 1 | no | `10` | Most workflow runs the GitHub Actions provider (pool `actions`) keeps running at once; further jobs of the pool stay queued. It bounds the Actions minutes forecrewd can spend (actions.md). |
| `fleet.currency` | ISO 4217 code | no | `USD` | Currency of every fleet amount: shape prices, budgets and usage costs (fleet.md). |
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
| `testing.projects` | list of project names | no | empty | Projects whose new goals run the `goal-v2` playbook: tests from the spec before the code, a test matrix run on every PR head and a verification report that gates the merge (playbooks.md). Every other project, and every goal that already runs, keeps the `goal` playbook. Names are the projects' names (`list_projects`); a name that is not a project yet does nothing. |
| `auth` | object | no | defaults below | Sign-in, sessions and tokens ([identity.md](identity.md); `config.Auth`). |
| `auth.session_idle` | Go duration | no | `168h` (7 days) | A browser session not used for this long ends. At most `session_absolute`. |
| `auth.session_absolute` | Go duration | no | `720h` (30 days) | A browser session ends this long after sign-in, used or not. |
| `auth.token_default_ttl` | Go duration | no | `2160h` (90 days) | Expiry of a personal or service account token created without one. |
| `auth.token_login` | bool | no | `false` | Legacy: lets `POST /api/v1/session` sign in with a token. Off, people sign in through a connection. |
| `auth.instance_admins` | list of strings | no | empty | The people who may open any organization's settings to recover access: verified emails (compared ignoring case) or `github:<user ID>`, the numeric ID. GitHub logins are refused, because a login can be renamed and then taken by someone else. Every use is audited. |
| `auth.connections` | map name → connection | no | empty | Instance-level sign-in connections, usable by every organization. Names are `domain.ValidName`s. forecrewd stores them at startup (`account_id` NULL). |
| `auth.connections.<name>.kind` | `github` \| `google` \| `microsoft` \| `oidc` | yes | | `github` is the Forecrew GitHub App's user authorization; the others are OpenID Connect. |
| `auth.connections.<name>.issuer` | URL | for `microsoft` and `oidc` | `https://accounts.google.com` for `google` | OIDC issuer, `https://`; empty for `github`. |
| `auth.connections.<name>.client_id` | string | yes | | OAuth client ID. |
| `auth.connections.<name>.client_secret_ref` | `env:NAME` \| `secret:name` | yes | | Where forecrewd reads the client secret: an environment variable, or the file `/run/secrets/<name>`. Never the secret itself, which is refused. Connections here may use the full forms; an organization's own connections are limited to `env:FORECREW_CONN_*` and `secret:conn-*` (identity.md). |
| `auth.connections.<name>.group_claim` | string | no | empty | ID token claim holding the user's groups, for group mapping (enterprise). |

Instance connections have no just-in-time provisioning (`jit` is not a
key here); an organization configures that on its own connections.

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
| `FORECREW_EMBEDDINGS_API_KEY` | no | OpenAI API key for knowledge embeddings (`text-embedding-3-small`, 1536 dimensions). Without it knowledge search ranks by words only. When set, and only where the database has pgvector, the text of knowledge entries and notes and every search query are sent to OpenAI (docs/knowledge.md). Never logged. |
| each `auth.connections.*.client_secret_ref` `env:NAME` | yes | Client secret of the connection, e.g. `FORECREW_GITHUB_CLIENT_SECRET`. Read when used, kept in memory only, never logged. |

forecrewd takes the config path from `--config` (default `forecrew.yaml`).

### Validation

`config.Parse` reports every problem at once, wrapping `config.ErrInvalid`:
missing or malformed `public_base_url`, `public_base_url` not matching
`tls_domain`, missing env secrets, `app_id` ≤ 0, no runtimes, unknown adapter,
missing vendor, invalid runtime name, a `base_url` that breaks its rules, an
`opencode` model not in `provider/model` form, unknown tier, role referring to an
unknown runtime, role tier without a model for the runtime's vendor,
reviewer/developer on the same vendor without `allow_same_vendor_review`,
invalid pool names, pools without kinds or with unknown kinds, empty role
names, pool runtimes that are not configured, unknown `repo_token` scopes,
and, when `evidence` is set, a malformed endpoint, no bucket, a negative
size or retention, and missing evidence keys; for `auth`, negative
durations, `session_idle` longer than `session_absolute`, connections with
an unknown kind, an invalid name, no client ID, an issuer that does not fit
the kind or a `client_secret_ref` that is not a reference, unset `env:`
secrets, and instance admins that are not an email or `github:<user ID>`.

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
| `registry_auth` | bool | no | `false` | Let the worker's Docker pulls and builds use the registry logins of the worker's user. By default they run with an empty Docker config, because image names come from repos and must not be able to use the operator's private registries. |
| `image_prune_days` | integer ≥ 1 | no | `14` | Days after which a worker with the `docker` label removes the environment images (`forecrew-env:*`) no job used. |

On top of `labels` the worker adds, when polling:

- its OS label: `linux`, `darwin` or `windows` (Go's `GOOS`; macOS also gets `macos`);
- `runtime:<adapter>` for each runtime CLI it finds (`runtime:claude-code`,
  `runtime:codex`, `runtime:opencode`);
- `docker` when the Docker engine answers `docker version`.

| Variable | Required | Meaning |
|----------|----------|---------|
| `FORECREW_WORKER_TOKEN` | yes | Worker token printed once by `forecrew worker add`. |

forecrew-worker takes the config path from `run --config` (default `worker.yaml`).
