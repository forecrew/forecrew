# Fleet

The fleet starts a machine for a job and throws it away. Machines come from
locations (capacity providers). A location is usually served by
`forecrew-fleet-agent`: a small daemon that runs where the machines are and
dials out to forecrewd, so the credentials of that place never leave it. The
same mechanism serves a home server, an on-premise rack and a customer's cloud.

The rules are in [contracts/fleet.md](contracts/fleet.md) and
[contracts/fleet-agent-api.md](contracts/fleet-agent-api.md). To connect a
location, see [runbooks/add-provider.md](runbooks/add-provider.md).

```
forecrew-fleet-agent run --config agent.yaml   # connect and keep the machines here as forecrewd asks
forecrew-fleet-agent doctor [--kind microvm]   # say what is missing for a kind of location
forecrew-fleet-agent detect                    # print the kind this machine looks like
forecrew-fleet-agent version
```

## Configuration

`agent.yaml`:

| Key | Required | Meaning |
|-----|----------|---------|
| `server_url` | yes | forecrewd's `public_base_url` |
| `kind` | yes | `microvm`, `proxmox`, `hyperv`, `tart` or `aws` |
| `token_file` | yes | Where a token got with a join code is kept, readable by the agent's user alone, and read at the next start |

| Variable | Meaning |
|----------|---------|
| `FORECREW_PROVIDER_TOKEN` | Token from `forecrew provider add`. It wins over `token_file`, and is not written to disk |
| `FORECREW_JOIN_CODE` | Code from `forecrew provider join`, used when there is no token yet |

Local credentials are not in this file. Each provider finds its own by the
machine's own means: files readable by the agent's user, or an instance role.
Nothing of them is ever sent to forecrewd: capabilities hold facts.

This build has no provider of a real kind yet (T61 to T64 add them): `run` says
so, and `doctor` lists it as missing.

## Connecting

There are two ways, and both end with an `active` location.

**A join code** shows no token to anyone. `forecrew provider join` draws a
single-use code that lasts 1 hour (`--cloud`: 7 days, for a kit another team
runs). An agent started with it reports what it found, with no access to
anything. `forecrew provider joins` shows that report and `forecrew provider
confirm <id> --name <name>` accepts it: the agent then gets its token over its
own connection and keeps it in `token_file`. What the agent reports is unchecked,
so look at the host and account before you confirm. An agent that restarts
before that, or loses the answer, needs a new code.

**`forecrew provider add <name> --kind <kind> [--org <slug>]`** prints a token
once, for scripts and air-gapped sites.

Without `--org`, both use the server's organization when it has only one.
On a server with several that means an instance-wide location, which only an
instance admin can make. Nobody can make a location for another organization.

`forecrew provider revoke <name>` stops the token at once and for good, a poll
that was waiting included. The agent logs why and stops; no service manager
restarts it, because its exit status is 0. Its machines are not touched at the
place, since nothing tells it to; forecrewd ends their rows, requeues their jobs
through their leases and revokes their workers. Destroy what is left at the
place by hand, or uninstall with the provider's own tools.

## What the agent does

It registers, reconciles, then long-polls the machines that should exist, and
acts on each answer:

- a machine it does not have, delivered with a token: `Create`, in the
  background so polls go on;
- a machine it has and that is not in the list: `Destroy`, and reports it;
- a machine whose tags, network or deadline changed: `Update`. A failure is
  tried again on the next poll and does not fail the machine.

A machine stays in `have` from the moment it is received, which is what keeps
its token from being replaced during a slow `Create`. When `Create` fails the
machine leaves `have` and the agent reports `failed` with the error; if that
report is lost, the machine comes back with a new token. No room
(`fleet.ErrCapacity`) is not reported: the agent waits 5 s and polls again.

Every 5 minutes, and after every reconnect, it lists what the provider holds.
forecrewd answers what to destroy. A reconcile waits for a running `Create`, so
that a machine it is starting does not look gone, and on the periodic round it
skips instead of waiting, so that polls go on.

**Offline.** While forecrewd cannot be reached the agent keeps its machines
running, creates and destroys nothing new, and still applies the deadlines it
knows. Reports wait in memory. After reconnecting it registers, reconciles and
polls. A restart loses nothing: what it has comes from the provider's list, and a
machine started just before a crash moves to `booting` with the token it has.

## Capabilities

`register` sends what the provider reports. Where it says nothing, the agent
fills in the host's name, OS, architecture, CPUs, memory and free disk, and it
adds the checks `memory`, `disk` and, on Linux, `kvm`. They are sent again when
they change, seen at the 5-minute round.

## Logs

The agent logs JSON to stderr. Neither side logs a body of this API, a provider
or bootstrap token, a join code or a claim, and errors of a provider are scrubbed
with `domain.SecretPatterns` before they are logged or reported.
