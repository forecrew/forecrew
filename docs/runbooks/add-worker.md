# Add and remove a worker

A worker is a machine that runs agent jobs. It only dials out to forecrewd.
It needs:

- outbound HTTPS to the instance's `public_base_url`, and to GitHub
  (`github.com`) and the model vendors' APIs;
- `git`;
- the runtime CLIs the jobs use, installed system-wide so the service user finds
  them (`/usr/local/bin` or `/usr/bin`; on macOS also `/opt/homebrew/bin`):
  `claude` (Claude Code), `codex` and/or `opencode` (exactly the version in
  [docs/runtimes.md](../runtimes.md), others are refused);
- the toolchains jobs build with (`go`, `node`, `docker`, ...). Advertise them
  as labels.

Linux is the recommended worker OS: it has the process isolation described
in [install.md](install.md#security-notes).

## Add

1. Register the worker on the instance, in its pool. The token is shown once:

   ```sh
   docker compose exec forecrewd forecrew worker add build-box-1   # pool build
   ```

   `--pool` picks the pool (default `build`, see
   [docs/worker.md](../worker.md#pools)): `build` for machines with the
   toolchains, `control` for read-only work (reviews, councils, triage) on a
   machine with only the agent CLIs: it never receives a write token. A pool
   defined under `pools` in `forecrew.yaml` needs that file:

   ```sh
   docker compose exec forecrewd forecrew worker add gpu-box-1 --pool gpu --config /etc/forecrew/forecrew.yaml
   ```

   Unknown pools and `actions` (served by GitHub-hosted runners) are refused.

2. On the worker machine, run the install script as root with that token.
   Do not type the token on a command line, in `--token` or in
   `sudo VAR=fcw_... sh ...`: both show it to every user of the machine in the
   process list, and your shell keeps it in its history. Read it without echo,
   export it, and let `sudo` pass that one variable on:

   ```sh
   curl -fsSLO https://raw.githubusercontent.com/forecrew/forecrew/main/deploy/worker/install.sh
   read -rs FORECREW_WORKER_TOKEN && export FORECREW_WORKER_TOKEN   # paste the token, Enter
   sudo --preserve-env=FORECREW_WORKER_TOKEN,GITHUB_TOKEN sh install.sh \
     --server https://forecrew.example.com \
     --name build-box-1 --labels go,node,docker
   unset FORECREW_WORKER_TOKEN
   ```

   (`GITHUB_TOKEN` is only needed for a private repository, see below; `sudo`
   passes only the variables that are set.) The token must be `fcw_` followed by
   letters, digits, `-` and `_`; the script refuses anything else.

   Options: `--work-dir DIR` (default `/var/lib/forecrew-worker/work`),
   `--max-jobs N` (default 1, see below), `--version TAG`.

   On Windows, in an elevated PowerShell:

   ```powershell
   $secure = Read-Host -AsSecureString 'Worker token'
   $env:FORECREW_WORKER_TOKEN = [System.Net.NetworkCredential]::new('', $secure).Password
   .\install.ps1 -Server https://forecrew.example.com -Name build-box-1 -Labels go,node
   ```

   See [Windows limits](#windows-limits) below.

The script:

- creates the dedicated user (`forecrew-worker`; `_forecrew-worker` on macOS) the
  service runs as;
- downloads the release for the OS and architecture and checks its checksum;
- prepares `/etc/forecrew-worker/worker.yaml` and the token file
  `/etc/forecrew-worker/token.env` (Windows: `C:\ProgramData\forecrew-worker`).
  On Linux the token file is owned by `root`, mode 0600: systemd reads it as
  root, and the agents, which run as the service user, cannot. On macOS and
  Windows it must be readable by the service user, so
  [agents can read the worker token there](#agents-and-the-service-user);
- runs `forecrew-worker check` against the new configuration and token, and
  stops if it fails, leaving the installed binary, configuration, token and
  service as they were. Only after it passes does it move them into place;
- installs and starts a systemd unit (launchd on macOS) that restarts the
  worker on failure.

3. Verify: the worker appears online within a minute.

   ```sh
   FORECREW_URL=https://forecrew.example.com FORECREW_TOKEN=fcm_... forecrew workers
   journalctl -u forecrew-worker -f     # macOS: tail -f /var/log/forecrew-worker.log
   ```

The labels the worker advertises are the ones you passed plus its OS
(`linux`, `darwin`/`macos`, `windows`) and `runtime:claude-code` /
`runtime:codex` / `runtime:opencode` for each CLI it finds on `PATH`.

### Private repository releases

If the Forecrew repository (and so its releases) is private, give the script a
GitHub token that can read the repository's releases in the environment:

```sh
read -rs GITHUB_TOKEN && export GITHUB_TOKEN
sudo --preserve-env=FORECREW_WORKER_TOKEN,GITHUB_TOKEN sh install.sh --server ...
```

(Windows: set `$env:GITHUB_TOKEN` the same way as the worker token.) The script
then downloads the release through the GitHub API instead of the public URL.
The variable is the only way to pass it: there is no flag. Read it as above, not
as `GITHUB_TOKEN=... sudo ...`, which would put it in the shell history. The
script hands it to `curl` through stdin, so it is not in the process list, and
never writes it to disk or to its output. It is sent to `api.github.com` only,
never to a mirror given with `--base-url`. Download the
script itself with the same token
(`curl -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: application/vnd.github.raw+json" https://api.github.com/repos/forecrew/forecrew/contents/deploy/worker/install.sh`).

### Windows limits

Known limits of the Windows worker in the MVP:

- It runs from a **scheduled task** started at boot, not as a Windows service:
  `forecrew-worker` is not a service executable. The task runs as the local user
  `forecrew-worker` (a fresh random password is set on every run of the script)
  and restarts the worker every minute while it fails. Stopping the task, as an
  upgrade does, ends the worker at once: its running jobs are not given the
  10-minute grace period and are requeued when their leases expire.
- `install.ps1` runs `forecrew-worker check` as the administrator who runs the
  script, not as the service user. A permission problem of the service user
  (a toolchain it cannot execute) shows up only in the task's behaviour:
  `Get-ScheduledTaskInfo -TaskName forecrew-worker`.
- Agents are less isolated than on Linux (see
  [install.md](install.md#security-notes)).
- The script has not been run on a real Windows machine yet: do the
  [first deployment checks](install.md#first-deployment-live-checks) there too.

### `max_jobs`

The default is 1. Raise it (`--max-jobs 2`) only for repos you trust equally:
the agents of concurrent jobs can read each other's repo tokens. The script
warns when you do. For isolation between repos, use one worker (one OS user,
ideally one machine) per trust level.

### Agents and the service user

Agents run as the service user and can read what it can read. On **Linux** the
token file is `root:root` 0600 and the worker's own environment is hidden from
its children, so agents cannot read the worker token. On **macOS and Windows
agents can read the worker token**: launchd and the scheduled task run the
service as that user, which therefore has to read the token file. A leaked
worker token lets someone lease jobs, and with them repo tokens, until you
revoke it. Use Linux workers where that matters. The user owns nothing else; do not add it to groups you would not
give to the code the agents run. Giving it access to Docker (the `docker`
group) is root-equivalent on that machine.

## Move to another pool

```sh
docker compose exec forecrewd forecrew worker move build-box-1 --pool control
```

The worker leases from the new pool at its next poll; the jobs it is running
finish where they started. The move is recorded in the audit log (`worker.pool_changed`, with the old and new pool). Nothing on the machine changes: the pool is never
taken from `worker.yaml` or the labels. Check the result in the POOL column of
`forecrew workers` or on the Workers page.

## Change the configuration

Running the script again upgrades the binary and keeps `worker.yaml`. To change
the labels, work directory or limits, edit `/etc/forecrew-worker/worker.yaml`
and run `sudo systemctl restart forecrew-worker`, or run the script with
`--reconfigure` and the new options. A new `--token` replaces the stored token
(use it after revoking and re-adding a worker).

## Remove

1. Revoke the token on the instance, so the machine can no longer lease jobs:

   ```sh
   docker compose exec forecrewd forecrew worker revoke build-box-1
   ```

   Its running jobs lose their leases and are requeued.
2. On the machine:

   ```sh
   sudo sh install.sh --uninstall           # keeps the config and the work dir
   sudo sh install.sh --uninstall --purge   # also deletes config, token, state and the user
   ```

   Windows: `.\install.ps1 -Uninstall [-Purge]`. A custom `--work-dir` stays;
   delete it yourself.

## Runtime keys

forecrewd normally sends each job its runtime's key (`credential_env`). A
runtime with an empty `credential_env` needs the key in the worker's
environment instead: `ANTHROPIC_API_KEY` for Claude Code, `CODEX_API_KEY` for
Codex, the provider's variable for opencode (`DEEPSEEK_API_KEY`, ...). A
`claude`, `codex` or `opencode` login on the machine is not used, because each
run gets an isolated config dir.
