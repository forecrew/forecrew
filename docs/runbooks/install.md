# Install forecrewd

forecrewd is a single binary with a Postgres database. It runs as **one
replica** in the MVP: do not start a second copy against the same database.
Workers do the agent work on other machines and only need outbound HTTPS to
the instance (see [add-worker.md](add-worker.md)).

You need:

- a Linux host with Docker and the Compose plugin (or systemd and Postgres 18);
- a domain name pointing at the host, unless a load balancer fronts it;
- a GitHub App for the installation (app ID, private key, webhook secret);
- API keys for the runtimes you use (`ANTHROPIC_API_KEY`, `CODEX_API_KEY`).

## With Docker Compose

```sh
git clone https://github.com/forecrew/forecrew && cd forecrew/deploy/compose
cp .env.example .env                    # fill in every value
cp forecrew.example.yaml forecrew.yaml  # edit, see below
chmod 600 .env
docker compose up -d
```

`.env` holds the secrets. Use a URL-safe `POSTGRES_PASSWORD` (letters and
digits, for example `openssl rand -hex 24`). Put the GitHub App private key
on one line in double quotes with `\n` for the line breaks:

```sh
awk 'NF{printf "%s\\n", $0}' app-key.pem   # prints the value for .env
```

`docker compose up` starts Postgres, runs `forecrew migrate up` once in the
`migrate` service, then starts forecrewd. A `backup` service dumps the database
daily (see [Backups](#backups)). The image is
`ghcr.io/forecrew/forecrewd:${FORECREW_VERSION:-latest}`; pin `FORECREW_VERSION`
in `.env` to a release tag for production. To build from a checkout instead of
pulling, add `-f docker-compose.build.yml` and `--build`.

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

### GitHub App

Create the GitHub App with the webhook URL `https://<your host>/webhooks/github`
and the webhook secret from `.env`. Install it on the repositories Forecrew
works on. Put its ID in `github.app_id` and the logins that may command
`@forecrew` in `github.allowlist` (empty means nobody).

### Check it

```sh
docker compose ps                                    # forecrewd "healthy", postgres running, migrate exited 0
docker compose exec forecrewd forecrew config validate --config /etc/forecrew/forecrew.yaml
curl -fsS https://forecrew.example.com/healthz       # ok
curl -fsS https://forecrew.example.com/ | grep -c 'id="root"'   # 1: the web UI is served
```

### Log in to the web UI

forecrewd serves the web UI at its public URL. Create an API token and paste it
on the login page (the same kind of token as for MCP, see
[mcp-token.md](mcp-token.md)):

```sh
docker compose exec forecrewd forecrew token create my-browser   # shown once
```

Open `https://forecrew.example.com/`, paste the token and log in. The server
keeps it in a `Secure`, `HttpOnly` cookie for 30 days; the page never stores it.
Use **Log out** in the sidebar, or revoke the token, to end the session. The
cookie needs HTTPS (or `http://localhost`), so behind a load balancer the
public URL must be `https://`. See [ui.md](../ui.md).

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
in double quotes across lines), `FORECREW_GITHUB_WEBHOOK_SECRET` and the
runtime credentials. systemd reads the file as root before it drops to the
`forecrew` user, so the file stays `root:root` 0600 and the service cannot read
or change its own configuration. The unit applies migrations before each start.

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
  `forecrew.yaml`. Only SHA-256 hashes of MCP, API and worker tokens are
  stored.
- Claude Code needs `ANTHROPIC_API_KEY` (it does not use a machine login) and
  Codex needs `CODEX_API_KEY`; forecrewd passes them to the worker per job.
