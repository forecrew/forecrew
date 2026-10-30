# Runbooks

Operations for a Forecrew installation, self-hosted or ours. The deployment
files are in [`deploy/`](../../deploy).

| Runbook | When |
|---------|------|
| [install.md](install.md) | First deployment of forecrewd (Compose or systemd), security notes, first checks |
| [upgrade.md](upgrade.md) | New release of forecrewd or the worker, backups and restore |
| [add-worker.md](add-worker.md) | Add, upgrade, remove a worker machine |
| [rotate-github-key.md](rotate-github-key.md) | Replace the GitHub App private key or webhook secret |
| [mcp-token.md](mcp-token.md) | Create an MCP/API token and connect a client |
| [troubleshooting.md](troubleshooting.md) | Job stuck queued, no matching worker, lease expiries, offline worker, webhook failures |
