# Troubleshooting

Start with the facts:

```sh
docker compose ps
docker compose logs --tail 100 forecrewd       # systemd: journalctl -u forecrewd -n 100
curl -fsS https://<host>/healthz               # 503 "database unavailable" means Postgres
forecrew jobs show <job>                       # status, attempts, events
forecrew jobs cancel <job>                     # stop a queued or running job
forecrew job artifacts <job>                   # files the job uploaded (reports, logs)
forecrew job tests <job>                       # test runs of a command job and the tests that failed
forecrew artifact get <id> -o file             # download one
```

forecrewd logs JSON. Look for `job lease expired`, `requeue expired jobs` and
webhook errors.

## Job stuck queued

A queued job waits until a worker leases it. Check, in order:

1. **Is any worker online?** `GET /api/v1/workers` (`online` is true when the
   worker polled in the last 60 s). None online: see [worker offline](#worker-offline).
2. **Does a worker match?** A worker leases a job only if it is in the job's
   pool and has every label the job needs plus `runtime:<adapter>` (for
   example `runtime:claude-code`). `job_status` then says
   `no online worker in pool <pool> with labels ...`. See
   [no matching worker](#no-matching-worker).
3. **Is it waiting out a retry backoff?** After a failed attempt that gave the
   job back, it waits 30 s doubling per attempt up to 10 minutes before it can
   be leased again. A lease that expired is requeued immediately.
4. **Are all workers busy?** Each runs `max_jobs` jobs (default 1). Add a
   worker or wait.
5. **Did the worker's account differ?** A worker leases only its own
   account's jobs.

## No matching worker

No online worker of the job's pool has the labels the job needs. Compare the
job's `pool` and `needs` (and its runtime) with the workers' `pool` and labels
in `GET /api/v1/workers`.

- Wrong pool: a worker leases only jobs of its own pool, whatever labels it
  has. Add a worker to the job's pool or move one there
  (`forecrew worker move <name> --pool <pool>`, see
  [add-worker.md](add-worker.md#move-to-another-pool)). A role pack's `pool`
  decides where its jobs run.
- Missing toolchain label (`go`, `node`, `docker`): add it to `labels` in the
  worker's `worker.yaml` and restart the service, or use another worker.
- Missing `runtime:claude-code`, `runtime:codex` or `runtime:opencode`: the CLI is not on the
  service's `PATH`. The service runs as the dedicated user with
  `PATH=/usr/local/bin:/usr/bin:/bin` (macOS also `/opt/homebrew/bin`). Install
  the CLI system-wide, then run
  `sudo -u forecrew-worker forecrew-worker check --config /etc/forecrew-worker/worker.yaml`
  with `FORECREW_WORKER_TOKEN` exported (read it with `read -rs`, then
  `sudo --preserve-env=FORECREW_WORKER_TOKEN ...`) to see the detected labels.
- Wrong OS label: the repo profile asks for `linux`, `macos` or `windows`.

## Lease expiries

`job lease expired` in the forecrewd log means a worker stopped heartbeating
for 5 minutes. forecrewd requeues the job (one more attempt) until
`max_attempts` (3 by default), then fails it.

- Worker machine down, asleep or offline: fix the machine; the job is retried.
- Worker restarted: `SIGTERM` lets running jobs finish for 10 minutes; a hard
  stop or reboot loses the lease. Expected after a power loss or a Windows
  upgrade.
- Network trouble between worker and instance: look for repeated 5xx or
  connection errors in the worker log (`journalctl -u forecrew-worker`). The
  worker also stops a job whose lease it cannot renew, and pushes nothing.
- Clock skew: check the worker's clock (`timedatectl`); the worker never
  extends a lease past the server's `lease_expires_at`.
- Jobs repeatedly expiring on one machine: out of disk in `work_dir`, or the
  agent exhausting memory. Check `df -h` and `dmesg`.

## Worker offline

`online: false` means no poll for 60 s.

```sh
systemctl status forecrew-worker                 # macOS: sudo launchctl print system/com.forecrew.worker
journalctl -u forecrew-worker -n 100             # macOS: tail /var/log/forecrew-worker.log
sudo sh -c '. /etc/forecrew-worker/token.env && export FORECREW_WORKER_TOKEN && exec runuser -u forecrew-worker -- forecrew-worker check --config /etc/forecrew-worker/worker.yaml'
```

Windows: `Get-ScheduledTaskInfo -TaskName forecrew-worker`.

`check` reports the failing part. Common causes:

- `401`: the token was revoked or mistyped. Re-add the worker
  (`forecrew worker add`) and run the install script with the new `--token`.
- Cannot reach the server: outbound HTTPS to `server_url` blocked, DNS, or the
  instance's certificate not valid yet (see the TLS section below).
- `git` or the runtime CLI missing.
- The service restarts in a loop: `journalctl` shows the error; the unit
  restarts it every 5 s on failure.

## Webhook failures

In the GitHub App's **Advanced** tab, **Recent Deliveries** shows the status
GitHub got.

- `401`/signature errors: `FORECREW_GITHUB_WEBHOOK_SECRET` differs from the App's
  secret. Fix and restart forecrewd, then redeliver.
- Timeout, connection refused, TLS error: GitHub cannot reach
  `https://<host>/webhooks/github`. Check DNS, the port GitHub reaches (443 in own-domain mode), the load balancer
  and the certificate.
- `2xx` but nothing happens: the commenter has no linked GitHub identity with an active membership in the project's organization, and the login is not in the deprecated `github.allowlist`
  (case-insensitive; empty means nobody), the App is not installed on the
  repository, or the repo is not a project. Look at the forecrewd log for the
  delivery.
- A burst of deliveries during an outage: redeliver the missing ones;
  forecrewd deduplicates by delivery.

## TLS and certificates (own domain mode)

Certificates come from Let's Encrypt over port 443 (TLS-ALPN-01). If the first
request fails:

- the domain must resolve to this host and port 443 must be open to the
  internet (cloud firewall too);
- `public_base_url` must be `https://<tls_domain>`;
- the `certs` volume must be persistent, or each restart asks for a new
  certificate and Let's Encrypt's rate limits apply.

`docker compose logs forecrewd` shows the certificate errors.

## Database

- `/healthz` returns 503: `docker compose ps postgres` and
  `docker compose logs postgres`.
- `migrate` exited non-zero: `docker compose logs migrate`; forecrewd does not
  start until it succeeds. Do not edit the schema by hand.
- Disk full: Postgres and the `backups` volume share the host's disk. Lower
  `BACKUP_RETAIN_DAYS` or move dumps off the host.

## Artifacts

- **A worker logs that the artifact was not uploaded, `503 unavailable`.**
  forecrewd has no `evidence` section in `forecrew.yaml`, or the file was not
  reloaded: add it, restart forecrewd, see
  [install.md](install.md#artifact-storage).
- **`413 too_large`.** The file is over `evidence.max_artifact_size` (100 MiB
  by default). Raise it or make the job write less.
- **`500` on upload or download, `put evidence` or `get evidence` in the log.**
  forecrewd cannot reach the bucket or the keys are wrong. Check
  `docker compose logs garage garage-init forecrewd`; `garage-init` must have
  exited 0 and `FORECREW_EVIDENCE_ACCESS_KEY_ID` / `..._SECRET_ACCESS_KEY` in
  `.env` must be the keys it imported.
- **A listed artifact answers `404` on download.** Retention deleted it
  (`evidence.retention_days`), or the object is gone from storage.
- **The bucket holds objects no artifact lists.** After a forecrewd crash
  during an upload or a failed delete of a replaced file. The daily pass
  deletes them once they are 24 hours old and writes `artifact.orphan_deleted`
  to the audit log; nothing to do. The log line `orphan object with an
  unexpected key kept` names an object under `jobs/` whose key has no
  account: remove it by hand.
