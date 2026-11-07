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

This build has the `microvm` provider (below); the other kinds come with T62
to T64. For a kind it does not have, `run` says so and `doctor` lists it as
missing.

## Connecting

There are two ways, and both end with an `active` location.

**A join code** shows no token to anyone. `forecrew provider join` draws a
single-use code that lasts 1 hour (`--cloud`: 7 days, for a kit another team
runs). An agent started with it reports what it found, with no access to
anything. `forecrew provider joins` shows that report and `forecrew provider
confirm <id> --name <name> --facts <hash>` accepts it: the agent then gets its
token over its own connection and keeps it in `token_file`. What the agent reports
is unchecked, so look at the host and account before you confirm. `--facts` names
the report you read: if the agent sent another since, the confirmation is refused
and you read it again. An agent that restarts
before that, or loses the answer, needs a new code.

**`forecrew provider add <name> --kind <kind> [--org <slug>]`** prints a token
once, for scripts and air-gapped sites.

Without `--org`, both use the server's organization when it has only one. A
server with several refuses that: name a slug, or `--org instance` for an
instance-wide location, which only an instance admin can make. So nothing becomes
instance-wide by accident, and an organization with the slug `instance` cannot be
named here. Nobody can make a location for another organization.

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

## Limits on joining

`join` is the one route without a token, so it is limited before any lookup, by
two limits:

- **Per address:** 2 calls a second with bursts of 20, from the address of the
  TCP connection. `X-Forwarded-For` is not read: anyone can write it, and a client
  would pick its own limit. So behind a reverse proxy every agent has the proxy's
  address and they share this limit, which is enough for dozens of waiting agents
  (each calls every 5 s), and a flood from one source through the proxy stops
  their joins too.
- **Global:** 5 calls a second with an unknown, used or expired code, over all
  addresses. This is what bounds guessing, and it is deliberately a lock-out:
  while someone floods `join` with wrong codes, past that rate **every** `join`
  gets `429`, real codes included. A real code uses up nothing, but it cannot get
  in during the flood. Agents that have their token are not touched, since only
  `join` is limited; joining agents back off and go on when the flood ends. It
  costs a new location its start, never a running one, and it is not a way to
  guess: at most about 3 million tries fit in a week-long code's life, against
  2^40 codes.

Both limits are in memory, and a restart of forecrewd resets them.

`reconcile` and `poll` refuse lists of more than 1000 entries with `400`.

## Logs

The agent logs JSON to stderr. Neither side logs a body of this API, a provider
or bootstrap token, a join code or a claim, and errors of a provider are scrubbed
with `domain.SecretPatterns` before they are logged or reported.

## MicroVM locations

A Linux host with KVM runs each machine as a fresh microVM, destroyed after its
jobs: under Firecracker and its jailer by default, under Cloud Hypervisor when
the shape asks for a GPU (or `provider_config: {vmm: cloud-hypervisor}`). Guests
are Linux; Windows guests are not offered yet.

What a machine is made of, all named in a record
(`<state_dir>/machines/<id>.json`) written before any of it, so that Destroy can
finish what a crash interrupted:

- **A jail.** Firecracker runs under the jailer in
  `<state_dir>/jail/firecracker/<id>/root`: its own chroot, mount namespace and
  user (`uid_base` + the machine's slot; nothing else may use those IDs), with
  Firecracker's seccomp filters. Cloud Hypervisor, which has no jailer, runs as
  that user on files only it can read, with its seccomp filters on. The VMM runs
  in its own session and outlives the agent.
- **Disks.** The root disk is the machine's own copy of the image, a reflink
  where the file system has them (btrfs, XFS: instant; elsewhere a full copy,
  which `doctor` warns about), sized to the shape's `disk_gb`, which is its quota:
  the guest grows its file system to it. Disks are sparse, so a machine is only
  started while the state directory's free space holds its disk and what the
  other machines may still write of theirs. The config disk holds the server's URL
  and addresses, the CA bundle and the bootstrap token; the guest wipes it once
  read. `<state_dir>/cache.ext4`, when it exists, is attached to every machine
  read-only at `/var/cache/forecrew` (make one with `mke2fs -d <dir>`); the agent
  makes the file read-only for everyone, as all machines share it.
- **CPU and memory** are the shape's, as vCPUs and guest memory. A host takes
  machines while their memory fits its own less 1 GB, up to `max_machines`.
- **A network.** A tap device of its own (`fcvm<slot>`) with a /30, no bridge,
  and an nftables table of its own (`inet forecrew_vm<slot>`): the machine reaches
  the internet through NAT, its resolver (`dns`) and forecrewd's address and port,
  and nothing else: not the host, the LAN or any network the host is connected
  to, other machines or metadata addresses. A location's `network_allow` (hosts
  or CIDRs) opens networks beyond the host, for a home or on-premise network;
  it never opens the host itself (which takes only forecrewd's port and DNS
  from a machine) or other machines. Machines have IPv4 only.
- **A wall-time kill.** At its deadline the agent kills the VMM and everything in
  its session, whether or not forecrewd can be reached; forecrewd then destroys
  the rest.
- **Docker inside.** An image with `dockerd` gets it started in the guest's own
  kernel before the worker, so environments with Docker work without touching
  the host. The guest kernel needs overlayfs, bridges, veth and netfilter.

Destroy kills the VMM and removes the table, the tap, the jail with every disk,
and the record. The agent lists machines from the records, so after a restart
it finds them all and forecrewd says which to destroy.

**Images.** A machine boots a `rootfs` image and its kernel, fetched over http(s)
into `<state_dir>/images` by digest. Before first use the agent checks the
signature against `trusted_keys_file` and the digests of both files
(`images.VerifyFile`), and refuses the machine otherwise. A cached file that is
not the one signed is removed and fetched once more. For an image stored
elsewhere (`s3`, `oci`, ...) put the file in that directory under its digest's
hex; it is checked the same way. Images unused for `image_cache_days` are
removed.

**Start time.** No snapshots yet: each machine boots its kernel, and its init is
a static program that needs no shell. Measured so far under QEMU with KVM, with
a general-purpose distribution kernel that loads its network driver as a
module, not under Firecracker: 2.6 s from starting QEMU to the worker's
bootstrap call on an idle host, 8 s on a loaded one. The Firecracker time, with
its minimal kernel, is still to measure (`make test-microvm` logs it).

### microvm.yaml

`/etc/forecrew-fleet-agent/microvm.yaml` (`FORECREW_MICROVM_CONFIG` moves it);
every key has a default and a missing file means all defaults. It holds no
secrets.

| Key | Default | Meaning |
|-----|---------|---------|
| `state_dir` | `/var/lib/forecrew-microvm` | Records, images and jails, on one file system; btrfs or XFS for instant clones |
| `trusted_keys_file` | `/etc/forecrew-fleet-agent/image-keys.pub` | Public keys images must be signed with, one `ed25519:...` per line |
| `firecracker`, `jailer`, `cloud_hypervisor` | the names on PATH | The commands |
| `uid_base` | `900000` | First of the users machines run as |
| `subnet` | `172.30.0.0/16` | Where the machines' /30 networks come from |
| `dns` | `1.1.1.1` | The machines' resolver |
| `max_machines` | from memory, 512 MB each | Machines at once |
| `gpus` | none | PCI addresses bound to `vfio-pci`, one per machine that asks for a GPU |
| `ca_file` | the host's bundle | What an https forecrewd's certificate chains to |
| `image_cache_days` | `14` | How long an unused image stays |

### Home server setup

1. A Linux host with KVM (`ls -l /dev/kvm`), ideally with `/var/lib` on btrfs or
   XFS.
2. Install Firecracker and its jailer from the Firecracker releases, both on
   PATH, and `ip` (iproute2) and `nft` (nftables). For a GPU, Cloud Hypervisor
   too, with the device bound to `vfio-pci` and listed in `gpus`.
3. Turn on forwarding: `net.ipv4.ip_forward=1` in `/etc/sysctl.d/`. Docker on the
   same host drops forwarded traffic it does not know; let the machines through
   with `iptables -I DOCKER-USER -i fcvm+ -j ACCEPT` and
   `iptables -I DOCKER-USER -o fcvm+ -j ACCEPT` (the machines' own tables still
   refuse the LAN).
4. Put the public keys you trust in `/etc/forecrew-fleet-agent/image-keys.pub`.
5. `public_base_url` of forecrewd must be an address machines can reach: not
   `localhost`. forecrewd on this host or the LAN is fine; machines may reach it,
   on its port only.
6. Install the agent with a join code ([runbooks/add-provider.md](runbooks/add-provider.md));
   for `microvm` it runs as root, as the jailer, taps and nftables need. Check
   with `forecrew-fleet-agent doctor --kind microvm`.
7. Add a shape on the location with a signed `rootfs` image, and `docker` in its
   labels if the image has Docker.
