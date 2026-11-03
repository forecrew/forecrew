# Upgrade

## forecrewd (Compose)

1. Read the release notes for configuration changes.
2. Take a backup now (the daily one may be a day old):

   ```sh
   docker compose exec backup /backup.sh
   ```

3. Set `FORECREW_VERSION` in `.env` to the new tag, then:

   ```sh
   docker compose pull
   docker compose up -d
   ```

   The `migrate` service applies schema changes first; forecrewd starts only if
   it succeeds. If it fails, `docker compose logs migrate` shows why and the
   old forecrewd keeps running.
4. Check `curl -fsS https://<host>/healthz` and `docker compose logs --tail 50 forecrewd`.

Roll back by setting the previous tag and running `docker compose up -d`. Schema
migrations are not reversed: if a release changed the schema, restore the
backup from step 2 (see [install.md](install.md#backups)) before starting the
older version.

Restarting forecrewd does not lose jobs: running jobs keep their leases on
the workers, and workers retry while forecrewd is down.

## Control worker (Compose)

The control worker uses the same `FORECREW_VERSION` as forecrewd. Pull and start
it with the rest of the stack in step 3; the `control-init` service does nothing
when the token volume already holds the worker's token, so the worker keeps its
registration. A stop sends `SIGTERM`, and the worker lets running jobs finish
for up to 10 minutes (`stop_grace_period` in `docker-compose.yml`). Its
`FORECREW_CONTROL_*` settings are in `.env` (see
[install.md](install.md#control-worker-compose)).

## Read-only roles need a control worker

From the version that runs `architect` and `product-owner` in pool `control`,
those jobs wait for a worker of that pool. Compose installs get one from
`docker compose up -d` (`worker-control`). Other installs must have one online
before or right after the upgrade: `forecrew worker add <name> --pool control`
and install that worker with its token ([add-worker.md](add-worker.md)), or
`forecrew worker move <name> --pool control` for a read-only worker. Architect
and product-owner jobs queued in the meantime stay queued and run once it is up;
nothing is lost.

## forecrewd (systemd)

Replace `/usr/local/bin/forecrewd` and `/usr/local/bin/forecrew` with the new
release's binaries and run `sudo systemctl restart forecrewd`; the unit
applies migrations before it starts.

## Workers

Run the install script again on each worker; it downloads the new release,
runs `forecrew-worker check` with it, replaces the binary and restarts the
service, keeping the configuration and the token:

```sh
sudo sh install.sh --version v0.3.0      # omit --version for the latest
```

The restart sends the worker `SIGTERM`: it stops polling and lets running jobs
finish for up to 10 minutes. On Windows the task is stopped, so running jobs
are cut off and requeued when their leases expire. Upgrade workers when no job
you care about is running if that matters.

You can upgrade forecrewd and the workers in either order: the worker API is
versioned by its path (`/worker/v1/`), and a release that breaks it says so in
its release notes. Upgrade forecrewd first when in doubt.
