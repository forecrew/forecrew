# Install forecrewd

forecrewd is a single binary with a Postgres database. It runs as **one
replica** in the MVP: do not start a second copy against the same database.
Workers do the agent work on other machines and only need outbound HTTPS to
the instance (see [add-worker.md](add-worker.md)).

You need:

- a Linux host with Docker and the Compose plugin (or systemd and Postgres 18);
- a domain name pointing at the host, unless a load balancer fronts it;
- a GitHub App for the installation (app ID, private key, webhook secret);
- API keys for the runtimes you use (`ANTHROPIC_API_KEY`, `CODEX_API_KEY`);
- S3-compatible storage for job artifacts (test reports, logs, screenshots):
  the Compose stack ships Garage, see [Artifact storage](#artifact-storage).

## With Docker Compose

```sh
git clone https://github.com/forecrew/forecrew && cd forecrew/deploy/compose
cp .env.example .env                    # fill in every value
cp garage.env.example garage.env        # the same for Garage's secrets
cp forecrew.example.yaml forecrew.yaml  # edit, see below
chmod 600 .env garage.env
docker compose up -d
```

`.env` holds the secrets. Use a URL-safe `POSTGRES_PASSWORD` (letters and
digits, for example `openssl rand -hex 24`). Put the GitHub App private key
on one line in double quotes with `\n` for the line breaks:

```sh
awk 'NF{printf "%s\\n", $0}' app-key.pem   # prints the value for .env
```

`garage.env` holds Garage's own secrets (see [Artifact storage](#artifact-storage)).
`docker compose up` starts Postgres and Garage, runs `forecrew migrate up` once
in the `migrate` service and `garage-init` once for Garage, then starts
forecrewd. A `backup` service dumps the database
daily (see [Backups](#backups)). The image is
`ghcr.io/forecrew/forecrewd:${FORECREW_VERSION:-latest}`; pin `FORECREW_VERSION`
in `.env` to a release tag for production. To build from a checkout instead of
pulling, add `-f docker-compose.build.yml` and `--build`. The `worker-control`
service runs read-only agent work next to forecrewd (see
[Control worker](#control-worker-compose)).

### Choose how HTTPS works

Edit `forecrew.yaml`:

**Own domain, automatic certificates.** forecrewd gets Let's Encrypt
certificates itself, proving ownership with TLS-ALPN-01 on port 443. Port 443
of the host must be reachable from the internet, and the domain must resolve
to the host before the first start. Port 80 is not used for certificates (the
Compose file publishes it anyway).

```yaml
server:
  public_base_url: https://forecrew.example.com
  tls_domain: forecrew.example.com
  cert_cache_dir: /var/lib/forecrew/certs   # the "certs" volume; keep it
```

Delete the `listen: ":80"` line (it then defaults to `:443`).

**Plain HTTP behind a cloud load balancer.** The balancer terminates TLS and
forwards to port 80 of the host. Leave `tls_domain` unset and keep
`listen: ":80"`; `public_base_url` is the balancer's `https://` URL, which
workers, GitHub and MCP clients use.

Putting the host on a private network (VPN, tailnet) is your choice and needs
nothing from Forecrew; workers must still reach `public_base_url`.

The forecrewd container's health check runs `forecrewd healthcheck`, which
asks the local `/healthz` (over HTTPS for the configured domain in own-domain
mode) and exits 0 or 1. Run it by hand with
`docker compose exec forecrewd forecrewd healthcheck --config /etc/forecrew/forecrew.yaml`.

### Artifact storage

Workers upload job artifacts (JUnit reports, logs, screenshots, later
verification reports) to forecrewd, which stores them in S3-compatible object
storage; users download them through forecrewd after logging in (never
directly from the bucket, and workers never get storage credentials). The
Compose stack runs a single-node [Garage](https://garagehq.deuxfleurs.fr/)
for this:

- `garage` is the server, configured by `garage.toml`. It listens only on the
  internal `storage` network (`garage:3900` for S3), which holds just forecrewd
  and Garage's own services: the control worker cannot reach it, and no port is
  published. It keeps its data
  in the `garage-meta` and `garage-data` volumes.
- `garage-init` runs `garage-init.sh` at every `docker compose up`: on first
  start it assigns the one-node layout, creates the bucket `forecrew-evidence`
  and imports the access key from `.env`, with read and write on the bucket
  only. On later starts it changes nothing.
- `forecrew.yaml` has an `evidence` section (the example file has it) with the
  endpoint `http://garage:3900`, that bucket, `max_artifact_size` and
  `retention_days`.

Before the first start generate the secrets (the commands are in the example
files). `garage.env` holds `GARAGE_RPC_SECRET` and `GARAGE_ADMIN_TOKEN`, read
only by the `garage` and `garage-init` services. `.env` holds the access key
`FORECREW_EVIDENCE_ACCESS_KEY_ID` (`GK` followed by 24 hex digits) with its
`FORECREW_EVIDENCE_SECRET_ACCESS_KEY` (64 hex digits): forecrewd reads these
two from the environment (`docs/contracts/config.md`) and never logs them, and
`garage-init` imports them into Garage. forecrewd never sees Garage's admin
token or RPC secret.

`retention_days` (default 0: forever; the example sets 90) makes forecrewd
delete artifacts older than that once a day, from storage and database, and
write each deletion to the audit log. Deleting an artifact that a verification
report points at keeps the report and clears its link to the rendered copy.
The same pass deletes objects under `jobs/` that no artifact points at and that
are over 24 hours old, whatever `retention_days` is.

Without an `evidence` section artifacts are not stored: uploads get
`503 unavailable` and workers finish their jobs without them. To use another
S3 server (AWS S3, MinIO, Cloudflare R2) instead of Garage, point `endpoint`,
`bucket`, `region` and `path_style` at it, put its keys in `.env`, create the
bucket yourself, and delete the `garage` and `garage-init` services (and their
`depends_on` entry and the `storage` network in `forecrewd`).

Garage's volumes are part of the installation's state: back up `garage-meta`
and `garage-data` along with the database dumps (a plain copy of the volumes
with Garage stopped is enough for one node), or the artifacts of old jobs are
lost with the host. The Postgres dump only has their metadata.

### GitHub App

Create the GitHub App with the webhook URL `https://<your host>/webhooks/github`
and the webhook secret from `.env`. Install it on the repositories Forecrew
works on. Subscribe the App to the events `issues`, `issue_comment`,
`pull_request`, `pull_request_review`, `check_suite`, `check_run`, `status` and
`push`: forecrewd reindexes a project's knowledge base on a push to its default
branch (the poller catches up every five minutes if you leave `push` out, but
cannot flag entries a push made out of date; see [../knowledge.md](../knowledge.md)).
Put its ID in `github.app_id`. `@forecrew` commands are accepted from members
of the project's organization who linked their GitHub account by signing in
with it, with the permission of the gate. `github.allowlist` is deprecated: it only
counts in an organization where nobody holds a role yet.

To show the verification of a pull request as the check run
`forecrew/verification`, the App also needs the **Checks** write permission.
Without it the verification is still posted as a PR comment, and forecrewd logs
a warning once (see [../playbooks.md](../playbooks.md#verification)).

To run command jobs on GitHub-hosted runners (pool `actions`), the App also
needs the Actions, Contents and Workflows write permissions and the
**Workflow runs** event; see [actions.md](../actions.md).

### Check it

```sh
docker compose ps                                    # forecrewd "healthy", postgres running, migrate exited 0
docker compose exec forecrewd forecrew config validate --config /etc/forecrew/forecrew.yaml
curl -fsS https://forecrew.example.com/healthz       # ok
curl -fsS https://forecrew.example.com/ | grep -c 'id="root"'   # 1: the web UI is served
```

### Sign-in connections

People sign in through connections, in `auth.connections` of `forecrew.yaml`
(see [config.md](../contracts/config.md)). Every client secret is named, never
written: `env:NAME` or `secret:name` (the file `/run/secrets/name`). The redirect
URL of each is `https://<your host>/auth/<connection ID>/callback`; forecrewd
stores the connections at startup, and the IDs are in the `login_url` of
`POST /api/v1/auth/discover`, or in the `connections` table.

- **GitHub** (`kind: github`): the user authorization of the Forecrew GitHub
  App. Use the App's client ID, generate a client secret, set the callback URL
  and give the App the read-only **Email addresses** account permission;
  without it people sign in only through an invitation by GitHub login or a
  linked identity.
- **Google** (`kind: google`): an OAuth client of type Web application.
- **Microsoft** (`kind: microsoft`): `issuer` is
  `https://login.microsoftonline.com/<tenant ID>/v2.0`, one tenant per
  connection. Entra's `email` claim is not verified, so it only counts with the
  optional claim `xms_edov`; add `email` and `xms_edov` to the app's ID token.
- **Any OpenID Connect provider** (`kind: oidc`): its `issuer`, with discovery.

An organization's own connections use the secret names `env:FORECREW_CONN_*` or
`secret:conn-*`, and link existing users only for domains it verified.

### First owner and sign-in

Add the first owner on the server, by email or GitHub login. The organization
created at install has the slug `default`:

```sh
docker compose exec forecrewd forecrew user add ana@example.com --org default --role owner
```

**Warning:** an email given to `forecrew user add` is treated as verified, on
your word. The first person to sign in with that address, through any instance
connection that calls it verified, becomes this user with this role. Check the
address before you run the command, and do not add one you do not control.

A GitHub login is turned into its stable user ID through GitHub's API, and needs
the GitHub connection stored (start forecrewd once first).

Open `https://forecrew.example.com/`, and sign in. From there people join through
invitations (`forecrew invitations create bea@example.com --role member`, or the
`create_invitation` tool): they open the link and sign in as the person it was
sent to. Someone with no user, membership or invitation is refused.

The session cookie is `Secure`, so it needs HTTPS (or `http://localhost`): behind a
load balancer the public URL must be `https://`. See [ui.md](../ui.md).

### Recovery

These work on the database with shell access to the server, are audited as
`user:<operator>`, and need no sign-in:

```sh
forecrew org create "Acme Labs"                       # then forecrew user add ... --org acme-labs
forecrew user disable ana@example.com                 # sessions and tokens stop at once
forecrew session revoke --user ana@example.com
```

People in `auth.instance_admins` (verified emails or `github:<user ID>`) may sign
in through an instance connection without a membership and switch to any
organization to recover access when its identity provider is down; each use is
audited there as `instance_admin.entered`. Taking someone off the list takes
effect when forecrewd restarts, in their existing sessions too.

### Control worker (Compose)

`worker-control` is a worker in pool `control` (docs/worker.md, Pools). It runs
the read-only roles that the role packs put there: `architect` and
`product-owner` today. Roles that write, and `reviewer` (it runs the tests),
stay on build workers (see [add-worker.md](add-worker.md)). A role may go to
`control` only when it has no write paths; a writing role in that pool is
refused when its job is created.

- **Image.** `ghcr.io/forecrew/forecrew-worker-control:${FORECREW_VERSION:-latest}`
  (`deploy/worker/control/Dockerfile`): `forecrew-worker`, git, and Claude Code
  and Codex CLI on Node. The CLI versions are exact in
  `deploy/worker/control/package.json`; `package-lock.json` pins their
  transitive dependencies with integrity hashes, and the image installs exactly
  that with `npm ci`. Base images are pinned by digest. To upgrade a CLI, change
  its version there and regenerate the lock with `npm install --package-lock-only`.
  No language toolchains. The worker and its agents run as user `forecrew`
  (uid 10001), with no capabilities and no new privileges.
- **Token.** `forecrew worker add` shows the token once and cannot be given
  one, so it is not in `.env`. The `control-init` service runs
  `forecrew worker add control --pool control` when the `control-token` volume
  has no token, and keeps the token there, readable only by root. The worker's
  startup reads it as root and drops to `forecrew` before the agents start, so
  they cannot read it. Running it again does nothing. If the worker is
  registered but the volume is gone, revoke it with `docker compose exec forecrewd forecrew worker revoke control` and run
  `docker compose up -d`.
- **Settings in `.env`.** `FORECREW_CONTROL_MAX_JOBS` (default 4) is how many
  jobs run at once. `FORECREW_CONTROL_SERVER_URL` (default `http://forecrewd:80`)
  is where the worker reaches forecrewd; the default works with `listen: ":80"`,
  which is the example file. **With your own domain certificates you must change
  it**: forecrewd then serves HTTPS on 443 only, so set it to
  `https://<your domain>`, which the worker reaches through the host's public
  address. Otherwise the worker cannot connect and stays offline.
- **max_jobs above 1.** Concurrent jobs can read each other's repo tokens (see
  [worker.md](../worker.md)). Control jobs get only read-only tokens, so the
  exposure is limited to reading the repos of the other control jobs. Raise
  `FORECREW_CONTROL_MAX_JOBS` only for repos you trust equally.
- **Networks.** Postgres is on `db`, an internal network. `control-init` and
  `migrate` join it too, and so does forecrewd, which also joins `edge` for its
  outbound HTTPS. `worker-control` is only on `edge`, which holds forecrewd and
  nothing else. The reason is that agents run in that container: an agent that
  could reach Postgres could read or change any job, token or account. The
  worker also has no database URL, no GitHub key and no runtime keys (forecrewd
  sends runtime keys with each job), and it reaches forecrewd's worker API only.
  The compose test (`deploy/tests/control_worker_test.sh`) checks that it cannot
  reach Postgres.
- **Other installs need one too.** Without this Compose service, add a worker
  in pool `control` with `forecrew worker add <name> --pool control` (see
  [add-worker.md](add-worker.md)), or move a read-only worker there with
  `forecrew worker move <name> --pool control`. Until one is online,
  `architect` and `product-owner` jobs stay queued, and `forecrew jobs show`
  (or `job_status`) says `no online worker in pool control`.
- **Check it.** `docker compose ps` shows `worker-control` running and
  `control-init` exited with 0. The web UI's Workers page lists `control` in pool
  `control`, online, with the labels `runtime:claude-code`, `runtime:codex` and
  `runtime:opencode`.

## With systemd

Use this instead of Compose when you run Postgres yourself.

```sh
# binaries from the release page: forecrewd and forecrew
tar -xzf forecrewd_linux_amd64.tar.gz forecrewd && sudo install forecrewd /usr/local/bin/
tar -xzf forecrew_linux_amd64.tar.gz forecrew && sudo install forecrew /usr/local/bin/
sudo useradd --system --home-dir /var/lib/forecrew forecrew
sudo install -d /etc/forecrew                                  # owned by root, as are the files in it
sudo install -d /var/backups/forecrew -o forecrew -m 700
sudo install -m 644 forecrew.example.yaml /etc/forecrew/forecrew.yaml   # edit; listen ":80" or tls_domain as above
sudo install -m 600 /dev/null /etc/forecrew/forecrewd.env      # owner root; write the variables below
sudo cp deploy/systemd/forecrewd.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now forecrewd
```

`/etc/forecrew/forecrewd.env` holds `FORECREW_DATABASE_URL`,
`FORECREW_GITHUB_PRIVATE_KEY` (a quoted value with `\n`, as above, or the PEM
in double quotes across lines), `FORECREW_GITHUB_WEBHOOK_SECRET`, the runtime credentials and, when you
configure `evidence`, `FORECREW_EVIDENCE_ACCESS_KEY_ID` and
`FORECREW_EVIDENCE_SECRET_ACCESS_KEY`. systemd reads the file as root before it drops to the
`forecrew` user, so the file stays `root:root` 0600 and the service cannot read
or change its own configuration. The unit applies migrations before each start.

For artifact storage without Compose, run any S3-compatible server (Garage,
MinIO, a cloud bucket), create the bucket and an access key limited to it, and
add the `evidence` section to `/etc/forecrew/forecrew.yaml`; see
[Artifact storage](#artifact-storage).

## Backups

Compose runs `deploy/backup/backup.sh` every 24 hours in the `backup` service:
a custom-format `pg_dump` into the `backups` volume, keeping
`BACKUP_RETAIN_DAYS` days (default 14). Copy the dumps off the host; a
backup that lives only on the machine it protects is not a backup:

```sh
docker compose cp backup:/backups ./forecrew-backups
```

With systemd, install the script as `/usr/local/bin/forecrew-backup` and enable
`deploy/systemd/forecrew-backup.timer` (needs `pg_dump` installed). The service
reads `/etc/forecrew/backup.env` (`PGHOST`, `PGUSER`, `PGDATABASE` and
`PGPASSFILE=/etc/forecrew/pgpass`); create the password file as
`host:port:database:user:password`, owned by `forecrew`, mode 0600. The
password never appears on the `pg_dump` command line.

Restore with the same major Postgres version. `--clean --if-exists` drops the
objects in the dump from the target database before it recreates them, so this
replaces the data in `forecrew` (to restore elsewhere, create the database
first and drop the two flags):

```sh
docker compose stop forecrewd
docker compose exec -T postgres pg_restore -U forecrew -d forecrew --clean --if-exists < dump-file
docker compose up -d forecrewd
```

## First deployment: live checks

Unit tests cannot cover these two; do them once on the first real
installation, with a worker connected ([add-worker.md](add-worker.md)):

1. **A real Codex run.** Codex's adapter was written against its documented
   event stream and has not been run against a real binary. On the worker
   machine, with `codex` installed and `CODEX_API_KEY` set in `.env`, make a
   `reviewer` job run (the reviewer role uses the `codex` runtime in the example
   config) and check that the job completes, events appear in the job
   (`forecrew jobs show <job>`) and the structured result is accepted.
2. **A real Claude Code denial.** Claude Code runs with an empty per-run
   `CLAUDE_CONFIG_DIR` and `--permission-mode dontAsk`, so a command that is not
   on the role's allow list must be refused. On the worker, with `claude`
   installed, run a job for a role whose `allowed_tools` has a narrow `Bash`
   rule (for example `Bash(git log *)`) and a prompt that asks the agent to run
   `curl https://example.com`. The job must show a denied tool call and no
   network request; if the command runs, stop and report it.

Note that the worker's machine must not rely on managed Claude Code settings to
widen permissions: managed settings still apply to the runs, and anything they
allow is allowed to agents.

## Security notes

- **Agents are not isolated from the worker.** They run as the worker's OS
  user, which is not a security boundary. Each worker has its own dedicated,
  unprivileged user (the install script creates it); do not run workers as a
  user that owns anything you care about. See [worker.md](../worker.md).
- **`max_jobs` stays at 1** unless every repo the worker serves is trusted
  equally: concurrent jobs can read each other's repo tokens.
- **Process isolation is Linux-only.** The environment hiding and the sweep of
  leftover agent processes work on Linux workers. macOS and Windows workers are
  weaker: the worker's environment may be readable by agents and leftover
  processes are not killed.
- **Agents can read the worker token on macOS and Windows.** On Linux the
  install script keeps the token file `root:root` 0600, out of reach of the
  agents, which run as the service user. On macOS and Windows the service user
  must read the file, so agents can too. Revoke the worker
  (`forecrew worker revoke`) if you suspect a leak. Prefer Linux workers for repos you do not control.
- **Prompts passed to `dispatch_job` are trusted.** They are rendered into the
  agent's instructions without filtering, by design. A manipulated MCP client
  can therefore send any instruction to an agent that has write access to the
  repo: protect MCP tokens like deploy keys and keep the number of tokens
  small.
- Playbooks are loaded from files at startup; only the playbooks in the image
  (or your own files) are run.
- Secrets live in `.env` or `forecrewd.env` with mode 0600, never in
  `forecrew.yaml`. Only SHA-256 hashes of MCP, API and worker tokens, sessions and
  invitation links are stored, and provider tokens are not kept at all.
- Claude Code needs `ANTHROPIC_API_KEY` (it does not use a machine login) and
  Codex needs `CODEX_API_KEY`; forecrewd passes them to the worker per job.
