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
    credential_env: [OPENAI_API_KEY]

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

policy:
  allow_same_vendor_review: false
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
| `runtimes.<name>.credential_env` | list of strings | no | empty | Names of env vars forecrewd reads at startup and passes to the worker in the job spec `env`. Each must be set. Empty means the worker machine provides credentials itself. |
| `model_tiers` | map tier → vendor → model | yes for every (tier, vendor) a role uses | | Tiers are `fast`, `strong`, `best`. |
| `roles` | map role → `{runtime, tier}` | no | empty | Runtime and tier per role. Role definitions (prompts, write paths, schemas) live in `packs/roles`. |
| `policy.allow_same_vendor_review` | bool | no | `false` | When false, `reviewer` and `developer` must use runtimes of different vendors. |

### Environment variables

| Variable | Required | Meaning |
|----------|----------|---------|
| `FORECREW_DATABASE_URL` | yes | Postgres URL, e.g. `postgres://forecrew:...@db:5432/forecrew?sslmode=require`. |
| `FORECREW_GITHUB_PRIVATE_KEY` | yes | GitHub App private key, PEM. |
| `FORECREW_GITHUB_WEBHOOK_SECRET` | yes | GitHub App webhook secret. |
| each `runtimes.*.credential_env` entry | yes | Runtime credentials, e.g. `ANTHROPIC_API_KEY`. |

forecrewd takes the config path from `--config` (default `forecrew.yaml`).

### Validation

`config.Parse` reports every problem at once, wrapping `config.ErrInvalid`:
missing or malformed `public_base_url`, `public_base_url` not matching
`tls_domain`, missing env secrets, `app_id` ≤ 0, no runtimes, unknown adapter,
missing vendor, invalid runtime name, unknown tier, role referring to an
unknown runtime, role tier without a model for the runtime's vendor, and
reviewer/developer on the same vendor without `allow_same_vendor_review`.

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
