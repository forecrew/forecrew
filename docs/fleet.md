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
forecrew-fleet-agent build-image               # build the Windows image of a Proxmox or Hyper-V location from your ISO
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

This build has the `microvm`, `proxmox` and `hyperv` providers, on a Mac the `tart`
provider, and the `aws` provider (all below). For a kind it does not have, `run` says
so and `doctor` lists it as missing.

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
- **The organization's cache.** With `org_cache_gb` set, a machine created
  for an organization (tag `forecrew:org`) gets that organization's cache of
  its pool as a disk of its own, mounted read-write at `/var/cache/forecrew-org`;
  an image points its tools there in `/etc/forecrew/env` (`GOMODCACHE`,
  `npm_config_cache`, a git reference directory). The host keeps one file per
  organization and pool, `<state_dir>/caches/<org>/<pool>.img`. Each machine
  gets its own copy (a reflink), opened on the host through dm-crypt
  (`aes-xts-plain64`) and passed to the VMM as a block device; when a machine
  whose VMM ran is destroyed its copy becomes the cache, the last one to end
  winning, unless the abuse checks stopped it. Nothing of one organization is
  given to another's machine: the cache a machine gets is chosen by the
  organization forecrewd assigned it, and opened under that cache's own key, so
  another organization's file would read as noise. A warm machine, which has no
  organization when it starts, gets no cache, also once it is assigned. A cache
  no machine took for `image_cache_days` is removed. A cache is shared by the
  jobs of one organization and pool, a trust zone, and by nobody else.

  **What the encryption protects against:** someone who copies the cache files
  (a backup, a disk taken out or sold, a snapshot of `state_dir`) without the key
  learns nothing from them. Each cache's key is derived from one host key,
  `cache_key_file` (by default `<state_dir>/cache.key`, made at first use, 32
  bytes, root only), which reaches `cryptsetup` on its standard input. Keep it
  on another mount than `state_dir`, or out of its backups, for that protection
  to hold: anyone who has both, or root on the running host, reads every cache.
  The encryption does not protect a cache from root on the host, from the
  organization's own jobs, which see it in plain, or against being changed: the
  agent only notices a file changed since it wrote it back (inode, size,
  modification or change time, kept next to it) and then starts that machine with
  an empty cache. A key that is damaged, or missing while caches exist, stops
  machines that need a cache with an error and is never made again, as that would
  lose every cache; restore it, or remove it together with `<state_dir>/caches`
  to start every cache empty.
- **CPU and memory** are the shape's, as vCPUs and guest memory. A host takes
  machines while their memory fits its own less 1 GB, up to `max_machines`, and
  while the kernel says it has the new machine's memory left (`MemAvailable`, less
  1 GB), whatever else took memory. Slots, memory, GPUs and disk are checked and
  taken under one lock with the machine's record, so two creates at once cannot
  both take the last room.
- **A network.** A tap device of its own (`fcvm<slot>`) with a /30, no bridge,
  and an nftables table of its own (`inet forecrew_vm<slot>`): the machine reaches
  the internet through NAT, its resolver (`dns`) and forecrewd's address and port,
  and nothing else: not the host, the LAN or any network the host is connected
  to, other machines or metadata addresses. A location's `network_allow` (hosts
  or CIDRs) opens networks beyond the host, for a home or on-premise network;
  it never opens the host itself (which takes only forecrewd's port and DNS
  from a machine), other machines, or link-local addresses, where clouds keep
  their metadata endpoints (169.254.169.254). Machines have IPv4 only, so IPv6
  (and `fd00:ec2::254`) is closed. With `egress: allowlist`, for a location of
  the instance that runs hosted machines, a machine reaches forecrewd, its
  resolver and `network_allow` only, which forecrewd fills with the hosted
  list, the organization's model endpoints and its `egress`
  ([contracts/fleet.md](contracts/fleet.md), Network); no entry of it opens a
  private network, a network of the host, the host, metadata or another
  machine, whatever it says. A machine of an instance-wide shape is refused,
  and fails with why, on a location whose `egress` is not `allowlist`: a hosted
  machine never reaches the whole internet. New connections to the usual mining-pool ports are
  counted, for the abuse checks.
  **Known limit: DNS.** A machine always reaches its resolver (`dns`) on port 53,
  in either mode, and a resolver answers names anyone can register, so a machine
  can send data out, slowly, inside DNS queries (DNS tunnelling) whatever its
  egress list says. Nothing here stops that; a hosted location that must, points
  `dns` at a resolver of its own that answers only the allowed names.
- **A wall-time kill.** At its deadline the agent kills the VMM and everything in
  its session, whether or not forecrewd can be reached; forecrewd then destroys
  the rest.
- **Abuse checks.** With `abuse` set, the agent looks at every machine about
  every 30 seconds: the CPU time of its VMM (its vCPUs are the VMM's threads)
  and the `mining` counter of its nftables table, which counts new connections
  to the usual ports of mining pools. A machine whose vCPUs stay over 90% busy
  for `mining_minutes` while it connects to those ports, or for `cpu_minutes`
  whatever it does, has its VMM killed at once; its cache is not kept. The agent
  reports it `failed` with `abuse` (`mining` or `cpu`), and forecrewd cancels its
  job instead of running it again, writes `machine.abuse` to the organization's
  audit log and opens a notice for the instance's operator (`forecrew notices`).
  Both are off by default; a location that runs hosted machines turns them on.
- **Docker inside.** An image with `dockerd` gets it started in the guest's own
  kernel before the worker, so environments with Docker work without touching
  the host. The guest kernel needs overlayfs, bridges, veth and netfilter.

Destroy kills the VMM and removes the table, the tap, the cache's dm-crypt
mapping (writing the copy back), the jail with every disk, and the record. The agent lists machines from the records, so after a restart
it finds them all and forecrewd says which to destroy.

**Images.** A machine boots a `rootfs` image and its kernel, fetched over http(s)
into `<state_dir>/images` by digest. Before first use the agent checks the
signature against `trusted_keys_file` and the digests of both files
(`images.VerifyFile`), and refuses the machine otherwise. It checks the digests
again before a machine uses files that changed since (another inode, size,
modification or change time), so a file damaged or replaced in the cache is
never booted unchecked; each machine gets its own copy of the kernel, so using a
file does not change it. A cached file that is not the one signed is removed and
fetched once more. For an image stored
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
| `cache_key_file` | `<state_dir>/cache.key` | The key the organizations' caches are encrypted with, absolute; put it on a mount of its own, out of the backups of `state_dir` |
| `org_cache_gb` | `0` | Size of each organization's cache disk, per pool; 0 gives machines none. Needs `cryptsetup` and `mkfs.ext4` |
| `abuse.cpu_minutes` | `0` | Stops a machine whose vCPUs stay over 90% busy this long; 0 is off |
| `abuse.mining_minutes` | `0` | Stops a machine whose vCPUs stay over 90% busy this long while it connects to the usual ports of mining pools; 0 is off |
| `hosted` | `false` | This location runs the instance's hosted machines. `doctor` then fails without `egress: allowlist` and warns while an `abuse` limit is off |
| `egress` | `open` | `open`: machines reach the internet and `network_allow`. `allowlist`: only `network_allow`, never a private network; for a location of the instance that runs hosted machines |

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

## macOS locations (Tart)

An Apple silicon Mac runs each machine as a clone of a signed Tart image, deleted
after its jobs. The fleet agent runs on the Mac as an ordinary service user
(`_forecrew-fleet-agent`), not as root, and the provider is in the darwin build
only. Guests are macOS arm64.

**Licences, and what follows from them.** Checked on 2026-11-07 against the
primary texts; read them again before you sell machines or upgrade macOS.

| What | The rule | Source |
|------|----------|--------|
| Two VMs per Mac | "up to two (2) additional copies or instances" of macOS "within virtual operating system environments on each Apple-branded computer you own or control", for software development, testing during software development, macOS Server or personal non-commercial use (section 2B(iii)) | Apple's licence for macOS Tahoe 26, `apple.com/legal/sla/docs/macOSTahoe.pdf`. The licence of macOS 27 was not found and is not checked |
| No service bureau | 2B(iii) "does not permit" using the virtual copies for "service bureau, time-sharing, terminal sharing, relay service or other similar types of services", except as section 3 permits | same |
| Renting a Mac | Section 3 allows leasing for "Permitted Developer Services" (continuous integration and the building and testing of software), each lease for at least 24 consecutive hours, the lessee having "sole and exclusive use and control" of the software and the hardware during the lease; the lessor must tell Apple first (`developer.apple.com/contact/macos-license`); the lessor or the lessee, not both, may run the two VMs (section 3D) | same |
| 24 hours on a cloud Mac | "Mac instances are available only as bare metal instances on Dedicated Hosts, with a minimum allocation period of 24 hours before you can release the Dedicated Host", one Mac instance per host | AWS, "Amazon EC2 Mac instances", docs.aws.amazon.com |
| Tart | The Tart repository's `LICENSE` is FSL-1.1-ALv2 (copyright 2022-2026 OpenAI): any purpose except a "Competing Use", a commercial product or service that substitutes for Tart or offers substantially similar functionality; each version becomes Apache 2.0 two years after its release. The licensing page on `tart.run` still describes the older Fair Source terms (free up to 100 CPU cores). The two disagree | `github.com/cirruslabs/tart` (now `openai/tart`), `tart.run/licensing` |

What the code does with them:

- **Never more than two.** `capabilities.max_machines` is at most 2 and
  `tart.yaml` refuses a larger `max_machines`, so the controller never asks for a
  third. Apple's limit is on the Mac, and `tart list` shows only the agent's own Tart
  home, so the provider counts the VMs of the whole Mac: the processes
  Virtualization.framework runs, one per VM (`com.apple.Virtualization.VirtualMachine`,
  from `ps`), whoever started them, Tart in another home or another tool. A VM of your
  own leaves room for one machine, and the location reports `max_machines` lower
  (never 0, which means no limit) with a warning check, `other VMs`. A machine
  Create refuses for that reason is `no capacity`, not a failure, and so is a
  `tart run` that dies with Tart's "The number of VMs exceeds the system limit", for VMs the count missed.
- **Your own Mac, your own jobs.** That is what the licence covers: a location
  belongs to an organization and runs that organization's jobs only. macOS images
  are never instance-wide (images.md), so Forecrew never offers hosted macOS
  machines of its own to others by the minute: that would be a service the
  licence does not allow outside section 3.
- **A rented Mac** is the organization's own location while it rents the host: you
  pay the whole 24 hours (set the shape's `note` to say so), and the Mac is for
  your jobs alone. A cloud Mac another team uses is a lease under section 3 that
  the team makes with you (a 7-day join code, `--cloud`, so they run the agent and
  hold the credentials); telling Apple is the lessor's duty.
- **Tart's terms** are for the owner to settle: running Tart for your own CI is an
  internal use. Whether selling machine time to others is a Competing Use is a
  legal question this repository does not answer.

**What a machine is made of**, in a record (`<state_dir>/machines/<id>.json`)
written before anything, so that Destroy can finish what a crash interrupted:

- **A VM** named `forecrew-<machine id>`, a clone (`tart clone`, copy-on-write on
  APFS) of the shape's image: a clean disk per job, never reused. `tart set` gives
  it the shape's CPU and memory, and its disk when the shape's is larger; the VM
  runs headless (`tart run --no-graphics --no-audio --no-clipboard`) in its own
  session, so it outlives the agent, and nothing of the host is shared: no
  `--dir`, no clipboard.
- **The settings of the worker** (`FORECREW_SERVER_URL`, the one-time bootstrap
  token) go in through the guest agent: `tart exec -i` runs a fixed script as root
  in the guest and the settings are its standard input, so the token is never an
  argument (the process list of the host would show it), never in a shared folder
  and never on the host's disk. It is written to `/var/run/forecrew/bootstrap.env`
  (mode 0700 root folder, on the machine's own disk) by a rename, so launchd, which
  starts `forecrew-start` when the file appears, never reads half of it. The
  worker's first act is to read and delete the file and to exchange the token
  ([images.md](images.md)); the disk is deleted with the VM.
- **A network.** With Softnet (the default) a machine reaches globally routable
  IPv4 addresses and nothing private: not the LAN. The Mac is kept out of reach
  twice over, because Softnet allows the VM network's gateway, which is the Mac, and
  every public address, which includes the Mac's own on its interfaces: the agent
  passes `--net-softnet-block=@host,<each IPv4 of the Mac's interfaces>/32`, loopback
  apart, read when the machine starts, and what is blocked beats what is allowed.
  `@host` is used because since Softnet 0.24 each VM has a network of its own, so no
  fixed gateway can be named. Because the gateway is also where a guest finds its DNS
  (the DHCP lease names it), the delivery script gives every enabled network service
  of the guest the resolvers of `dns` (default `1.1.1.1` and `9.9.9.9`); a resolver of
  a private address is added to the allowed addresses, and one that is the Mac's own,
  loopback, link-local or in `100.64.0.0/10` (Tailscale's range, reached through the
  Mac) is refused. Before any setting is delivered, the agent asks the guest agent
  for the machine's address (`tart ip --resolver agent`: Softnet serves DHCP itself,
  so the default resolver sees nothing) and requires the Mac to have an address on
  that network; if it has none, the block of the Mac cannot be confirmed, and the
  machine is refused. The agent also adds forecrewd's address (resolved on the host;
  a loopback address, or any address of the Mac, is refused) and the location's
  `network_allow` as `--net-softnet-allow`, which opens those addresses on all ports,
  Softnet having no ports. A running VM's network cannot change, so `Update` with
  another `network_allow` fails. `softnet: false` is Tart's NAT, where a machine
  reaches the LAN and the Mac: `doctor` fails it unless `allow_no_softnet: true` is
  set on the Mac, and then only warns. `doctor` also requires Softnet
  `min_softnet_version` (`softnet --version`).
- **A wall-time kill.** At the deadline the agent runs `tart stop`, whether or not
  forecrewd can be reached, and again after a restart; forecrewd then destroys the
  rest.

Destroy stops the VM, deletes it (`tart delete`) and removes the record. `List`
returns the records and every VM named `forecrew-<uuid>` that has none, which
forecrewd finds to be orphans; a record whose VM was never made is listed and
destroyed the same way. Nothing else on the Mac is touched.

**Images.** A shape's image has format `tart`, `uri` `oci://registry/name[:tag]`
and `digest`, the digest of the build's OCI manifest. Before first use the agent
checks the signature against `trusted_keys_file`. The signature covers the digest
and not the URI, so the URI's host must be in `registry_hosts` of `tart.yaml`
(required when there are credentials), and credentials go only to that host. It
pulls `registry/name@<digest>`, never a tag. **Tart does not check that what it
pulled is what the digest names, so the agent does**: the `manifest.json` Tart
cached (`TART_HOME/cache/OCIs/<registry>/<repository>/<digest>/`, the registry's
bytes) must hash to the signed digest; `config.json` and `nvram.bin` must match
their layers' digests; and `disk.img`, which Tart stores uncompressed, must match
the layers' uncompressed content digests chunk by chunk and be covered by them
exactly. A layer that cannot be checked, such as another media type or a stacked
disk, refuses the image, and so does a pull that left no manifest. The disk is hashed
once per image, not per machine, and again if its files change or the cache is
pruned. The agent keeps its lock on the cache until `tart clone` returns, so nothing
prunes or pulls over a checked image, and compares the files' fingerprint once more
right before the clone; a change in between refuses the machine. A stacked disk (the
block size is on the manifest) is refused, since it is not one flat file. Pulled images unused for `image_cache_days` are pruned
(`tart prune --entries caches`). A shape's `provider_config` `base_vm` names a VM
in the agent's own Tart home (`<state_dir>/tart-home`, which `tart list` shows the
agent's user and no other: a VM of your own account's `~/.tart` is not there)
instead, only when `allow_local_base: true` is set in `tart.yaml` on the Mac
itself: such a VM is not signed, so only the person at the Mac can allow it.

**Xcode.** An image lists the Xcode of its base VM as a label, `xcode:16` (major
version; `provision-macos.sh` adds it when `xcodebuild` works). Put the label in
the shape's `labels` and write `needs: [xcode:16]` in the job; the worker gets it
with `macos`, `darwin` and `arm64`. A base VM with only the command line tools
has no Xcode label.

**Credentials stay on the Mac.** A private registry's credentials are named in
`tart.yaml` by reference (`registry_username: env:NAME`,
`registry_password: env:NAME`) and read from the agent's environment, the
`secrets.env` the installer reads. They reach `tart pull` and `tart clone`, and
only those, as `TART_REGISTRY_USERNAME`, `TART_REGISTRY_PASSWORD` and
`TART_REGISTRY_HOSTNAME` (the image's host, which must be in `registry_hosts`), in
an environment built for the command: the rest of the agent's environment, with its
provider token, never reaches Tart. The password is scrubbed from every error.
There is no SSH: the agent reaches a guest with the guest agent (`tart exec`), so
there are no SSH keys to keep.

**Untrusted text.** What a guest says (the output of `tart exec`) and what Tart
prints reach forecrewd only as a quoted, one-line snippet of at most 300 bytes,
scrubbed of secret shapes (`fleet.CleanError`), so it is inert data in a check or
a machine's error.

**Checks** (`forecrew-fleet-agent doctor --kind tart`, and the location's
capabilities): Apple silicon; Tart at least `min_tart_version`; free disk of
`min_free_gb`; trusted image keys; sleep off (`pmset`, a warning: run
`sudo pmset -a sleep 0`); the registry credentials the references name; Softnet on
PATH and on (a fail when off, unless `allow_no_softnet`); and, in the capabilities,
the VMs of the Mac that are not Forecrew's.

### tart.yaml

`/etc/forecrew-fleet-agent/tart.yaml` (`FORECREW_TART_CONFIG` moves it); every key
has a default and a missing file means all defaults. It holds no secrets.

| Key | Default | Meaning |
|-----|---------|---------|
| `state_dir` | `/var/lib/forecrew-fleet-agent/tart` | Records, and Tart's own storage (`TART_HOME` is `<state_dir>/tart-home`) |
| `trusted_keys_file` | `/etc/forecrew-fleet-agent/image-keys.pub` | Public keys images must be signed with |
| `tart` | `tart` | The command |
| `max_machines` | 2 | Machines at once, 1 or 2: Apple's limit |
| `min_tart_version` | `2.27.3` | The oldest Tart accepted. Chosen, not derived: 2.27.3 removed a 1-second limit on calls to the guest agent |
| `min_free_gb` | 100 | Free disk the host must keep |
| `boot_wait_seconds` | 300 | How long Create waits for a guest agent |
| `softnet` | `true` | Run machines behind Softnet. Off is a `doctor` fail unless `allow_no_softnet` |
| `allow_no_softnet` | `false` | Let this Mac run without Softnet: machines then reach the LAN and the Mac, and `doctor` warns |
| `min_softnet_version` | `0.25.0` | The oldest Softnet accepted (`softnet --version`; 0.23 added the flag, 0.24 gave each VM its own network, 0.25 is the newest release whose notes I read). Chosen, not derived |
| `dns` | `1.1.1.1`, `9.9.9.9` | Resolvers set in each guest, one to three IPv4 addresses, because the Mac is blocked: not the Mac's own address, loopback, link-local or `100.64.0.0/10` |
| `registry_username`, `registry_password` | none | `env:NAME` references; both or neither |
| `registry_hosts` | none | Registries an image may come from, `host[:port]` in lower case; an image of another host is refused. Required with credentials, and worth setting always |
| `allow_local_base` | `false` | Let a shape clone a local VM (`base_vm`) |
| `image_cache_days` | `14` | How long a pulled image unused stays |

### What a Mac needs

1. An Apple silicon Mac with macOS 26 or newer (Softnet needs it), signed in to
   nothing, set never to sleep (`sudo pmset -a sleep 0`).
2. `tart` and `softnet` on PATH (Tart: `brew install cirruslabs/cli/tart`;
   Softnet from its releases, with the setuid bit or passwordless sudo, see its
   README). Install them yourself, from a source you trust; the agent never
   downloads either.
3. Trusted image keys in `/etc/forecrew-fleet-agent/image-keys.pub`.
4. `public_base_url` of forecrewd reachable from the Mac's machines, not
   `localhost`.
5. The agent with a join code ([runbooks/add-provider.md](runbooks/add-provider.md)).
6. A shape with `os: macos`, `arch: arm64`, `max_jobs: 1`, a `tart` image and the
   `xcode:NN` label the image has.

Tested against a fake `tart` only; see [runbooks/add-provider.md](runbooks/add-provider.md)
for the live checks, and what is not yet verified on a real Mac.

## Proxmox locations

The agent runs on a Linux host next to Proxmox VE (a node is fine) and drives
its API. Each machine is a VM **linked-cloned from a template** (a full copy when
the shape names a storage), started, given its bootstrap settings and destroyed
after its jobs. Linux and Windows guests, `qcow2` images.

- **Credentials.** An API token in `/etc/forecrew-fleet-agent/proxmox-token`
  (`user@realm!name=secret`), readable by the agent's user alone; the agent
  refuses a file others can read, and the token is never sent to forecrewd,
  logged, or put in an error. Give the token a role of its own on `/vms`, the
  storage and the nodes it uses: `VM.Allocate`, `VM.Clone`, `VM.Config.CPU`,
  `VM.Config.Memory`, `VM.Config.Disk`, `VM.Config.Network`,
  `VM.Config.Options`, `VM.PowerMgmt`, `VM.Audit`, `VM.GuestAgent.FileWrite`
  (`VM.Monitor` on Proxmox VE 8), `VM.GuestAgent.Audit`, `Datastore.AllocateSpace`,
  `Sys.Audit`, and `Pool.Allocate` for a pool. Privilege names are of Proxmox VE 9.
  **Scope the token to a resource pool** (`/pool/forecrew`, with `VM.Clone` on the
  template and the privileges on the storage it needs) and name the pool in the
  shapes' `provider_config`, so that the token can reach the machines it makes and
  nothing else of the cluster.
- **The API's certificate is always checked.** There is no setting to skip it. A
  self-signed certificate is trusted by naming its CA in `ca_file` (the cluster's
  is `/etc/pve/pve-root-ca.pem`, which the installer copies), and
  `tls_server_name` is the name it must carry when the API is reached by an
  address. `api_url` must be `https`.
- **Which VMs are Forecrew's.** Every VM the agent makes has notes (the
  description) that only it writes: the machine's ID, its tags, network allowlist
  and deadline, never a secret. The agent lists, changes and destroys VMs by
  those notes and a `forecrew-` name, on the nodes of `proxmox.yaml`, and refuses
  any other VM even by its ID (a VM whose name is not `forecrew-...` too). What the hypervisor sends back (notes, error
  texts) is read strictly, shortened and scrubbed of secrets.
- **Templates and images.** A shape's `provider_config` names the template:
  `{node, template, storage, pool, disk, bridge, vlan}`. Import the image file
  into a VM, convert it to a template and give it the tag the agent says when it
  refuses (`forecrew-image-` and the first 32 hex digits of the image's digest).
  The agent checks the image's signature against `trusted_keys_file`, and that
  the template carries that tag: the tag is the operator's statement that the
  template was made from the signed file (the agent cannot open a template's
  disk), and it keeps a shape from cloning any other VM. The disk is grown to the
  shape's `disk_gb`; a template disk bigger than the shape is refused.
- **Bootstrap token.** Through the QEMU guest agent, once the guest answers: the
  agent writes `/run/forecrew/bootstrap.env` (Linux) or
  `C:\ProgramData\Forecrew\run\bootstrap.env` (Windows) into the running guest.
  The token never reaches a file of the Proxmox host, the VM's configuration or a
  cloud-init drive. The image needs the guest agent (`qemu-guest-agent` in the
  Linux image; the virtio-win guest tools in the Windows one, built with
  `guest_tools_installer`), and the agent's wait for it is the shape's boot
  timeout.
- **Network.** The VM's device is on `bridge` (and `vlan`) with the VM firewall
  on: inbound closed, the MAC filter on, and the output policy **DROP**, so that
  IPv6, which machines do not have, is closed. (`ipfilter` is not set: without an
  `ipfilter-net0` set, pve-firewall drops all of a VM's IPv4 traffic, DHCP
  included.) Outbound is explicit IPv4 rules, in this order: forecrewd's address
  and port, the resolvers in `dns`, then **never the cluster or the machines'
  network, whatever `network_allow` says, even `0.0.0.0/0`**: the address of every
  node in `GET /cluster/status` (so the API on 8006 and SSH), the host of
  `api_url`, and the network of `bridge` in `GET /nodes/<node>/network` (its
  `cidr`), which holds the other machines. These come from the API, not from the
  agent's own interfaces, so they hold when the agent runs off a node. **A machine
  is not made, and an allowlist not changed, if any of them cannot be read** (the
  token needs `Sys.Audit`), the API's host does not resolve, or the bridge has no
  address on the machines' network. With no `dns` listed, port 53 is open to any
  address, after what is never opened. Then `network_allow`, private,
  link-local and shared ranges and the node's other networks (the allowlist may
  open those, as in a microVM), and the internet last. A node that holds forecrewd
  is reached on forecrewd's port only.
  **The datacenter firewall must be on** (Datacenter, Firewall, Options): without it
  the rules do nothing, so `doctor` fails while it is off and **Create refuses to
  make a machine** until it is on. A template with network devices besides `net0`
  is refused (a clone would keep them, on whatever bridge they name).
- **Limits and the kill.** `max_machines` Forecrew VMs in the cluster and the
  node's free memory; the check and the clone are one step, so parallel starts
  cannot go over the limit. A machine's deadline is kept in its notes; at the deadline
  the agent powers the VM off, and the listing at each reconcile arms it again
  after a restart.
- **Destroy** stops the VM and removes it with its disks (`purge`,
  `destroy-unreferenced-disks`).

### proxmox.yaml

`/etc/forecrew-fleet-agent/proxmox.yaml` (`FORECREW_PROXMOX_CONFIG` moves it). The
installer writes `nodes`, `ca_file` and `tls_server_name`. It holds no secrets.

| Key | Default | Meaning |
|-----|---------|---------|
| `api_url` | `https://localhost:8006` | The API, https only |
| `token_file` | `/etc/forecrew-fleet-agent/proxmox-token` | The API token |
| `ca_file` | the system's roots | What the API's certificate chains to |
| `tls_server_name` | the host in `api_url` | The name the certificate must carry |
| `trusted_keys_file` | `/etc/forecrew-fleet-agent/image-keys.pub` | Public keys images must be signed with |
| `nodes` | required | The nodes machines may be made on; a shape names one |
| `bridge`, `vlan` | `vmbr0`, none | The machines' network; a shape may name its own |
| `dns` | any resolver | Resolvers machines may use |
| `max_machines` | `8` | Forecrew VMs at once |
| `poll_interval` | `2s` | Spacing of the polls of a task or the guest agent |

## Hyper-V locations

The agent runs on a Windows host with Hyper-V, as a member of Hyper-V
Administrators (`install.ps1` does this), and drives it through PowerShell
scripts embedded in the agent. Each machine is a VM whose OS disk is a
**differencing disk of a base `vhdx`** (made once per image, read-only) and which
has a small **config disk** of its own; when its jobs are done the VM, both disks
and its directory are **deleted**. A differencing disk makes a new machine as
cheap as restoring a checkpoint, and nothing of the last job survives, so there
is no checkpoint reset; checkpoints are turned off. Windows guests, `vhdx`
images. A differencing disk cannot be larger than its parent, so a machine's disk
is the image's size and `disk_gb` must be at least that.

- **Closed folders.** Before the agent does anything else on the host it closes the
  state, machine, image and script folders to SYSTEM, Administrators and its own
  account, and checks the result; **it refuses to run while one is open to anyone
  else**, because a machine's config disk, which holds its token, is under them
  (the disk's own folder is checked again before the token is written to it). The
  default `state_dir` is `C:\ProgramData\forecrew-fleet-agent\state\hyperv`, under
  the folder `install.ps1` closes. The step runs from the agent's memory, not from
  a script file, so it cannot be replaced in a folder that was open.
- **Scripts.** The PowerShell text is fixed; what the provider is given (names,
  paths, the token) goes in on standard input as JSON, never on a command line or
  into a script, so no value is run as code. Scripts act on a directory made from a
  machine's ID, inside `vm_dir`, and on a VM **by its ID**, never its name, which an
  operator's own VM may share: such a VM is never stopped, changed or removed. A
  failed start removes only what it made.
  What a script prints is shortened and scrubbed before it is shown.
- **Images.** The agent fetches the base image over http(s) into `image_dir`
  under its digest, and checks the signature against `trusted_keys_file` and the
  file against the digest before first use (`images.VerifyFile`), as for a
  microVM, and again whenever the cached file's size or time of change is not what
  it was when it was checked. An image built on this host (`build-image`, below) is put there
  already. Images unused for `image_cache_days` are removed.
- **Bootstrap token.** On a **config disk** made for the machine: a 32 MB virtual
  disk labelled `FCCONFIG`, formatted for it, holding `bootstrap.env`. It is the
  machine's own disk, it goes with it, and `forecrew-start.ps1` in the guest copies
  the file into the guest, overwrites it on the disk and deletes it. The disk is
  mounted on the host for a moment inside the machine's directory, never on a drive
  letter. Nothing is written elsewhere on the host, and Hyper-V PowerShell Direct
  and WinRM are not used (the image has no remote login).
- **Network.** The device is on `switch` (and `vlan`) with DHCP and router guard
  on and MAC spoofing off, and extended port ACLs. Outbound, in order: forecrewd's
  address and port, the resolvers in `dns`, then **never the host or the network of
  the switch, whatever `network_allow` says, even `0.0.0.0/0`**: every address of
  the host (its management and RDP included) and the network of the host's
  `vEthernet` adapter on the switch, which holds the other machines. Then
  `network_allow`, private, link-local and shared ranges and the host's other
  networks. Rules for a protocol and port (forecrewd, DNS) are stateful, which lets their answers in
  from forecrewd and from a resolver on the LAN; an allowlist entry is allowed
  outbound only, as stateful TCP and UDP. Nothing is allowed inbound by the sender's
  port, which a sender chooses: the only inbound allow is the DHCP reply, to the
  client port 68 from port 67. Only the denials are mirrored inbound, so no other private address may start a
  conversation with a machine. **IPv6 is denied both ways at the lowest weight**:
  machines have IPv4 only. A switch of its own, with NAT, keeps machines off the
  LAN best; the ACLs are a second wall.
- **Worker in the guest.** The image starts the worker from the scheduled task
  `ForecrewWorker` at boot, as SYSTEM, not as a Windows service: `forecrew-worker`
  does not implement the service control protocol (docs/worker.md). The task
  runs `forecrew-start.ps1`, which waits up to 10 minutes for the settings, reads
  them as data, and powers the machine off when the worker ends. The MVP's limit
  that made changes necessary was that this script read the settings the moment
  the file existed: it now waits until nobody holds the file open for writing
  (the guest agent writes it in place) and also reads the config disk.
- **The kill.** A deadline is kept in the VM's notes; the agent powers the VM off
  at it, and arms it again from the listing after a restart.

### hyperv.yaml

`C:\ProgramData\forecrew-fleet-agent\hyperv.yaml` (`FORECREW_HYPERV_CONFIG` moves
it). It holds no secrets.

| Key | Default | Meaning |
|-----|---------|---------|
| `switch` | required | The virtual switch machines connect to |
| `vlan` | none | The device's VLAN; a shape may name its own |
| `state_dir` | `C:\ProgramData\forecrew-fleet-agent\state\hyperv` | Scripts, and by default the images and the VMs |
| `vm_dir`, `image_dir` | under `state_dir` | One directory per machine; the base images |
| `trusted_keys_file` | `C:\ProgramData\forecrew-fleet-agent\image-keys.pub` | Public keys images must be signed with |
| `generation` | `1` | 1 boots the BIOS images the template builds |
| `dns` | any resolver | Resolvers machines may use |
| `max_machines` | `2` | Machines at once |
| `image_cache_days` | `14` | How long an unused image stays |
| `powershell`, `script_timeout` | `powershell.exe`, `10m` | The command and the time of one script |

A shape's `provider_config` may hold `switch` and `vlan`.

## Windows images on a host

Forecrew cannot hand out Windows, so the owner builds the image on the host that
will run it, from an ISO they are licensed for:

```
forecrew-fleet-agent build-image --templates <deploy/images of this release> \
  --iso <abs path> --iso-checksum sha256:<hex> --license-confirmed \
  --chocolatey-sha256 <hash of the install.ps1 you reviewed> --worker <forecrew-worker.exe> \
  --version 2026.11.1 --uri-base https://images.example.com --out <abs dir> \
  [--guest-tools virtio-win-guest-tools.exe --guest-tools-sha256 <hash>]   # Proxmox
  [--cache-dir <image_dir> --switch <switch>]                              # Hyper-V
```

It checks the licence statement, the ISO against the checksum you gave (read
again here), that the worker is a Windows amd64 program and that the disk has 100
GB, then runs Packer with `deploy/images/windows` (QEMU on Linux, Hyper-V on
Windows): about an hour. Its variables go in a file only the agent's user reads,
and it writes `progress.json` (step, state, error) and `build.log` in `--out`. The
result is an **unsigned manifest**: a person checks it, signs it with a key that
stays off the host, registers it, and puts the public key in the host's
`image-keys.pub` (docs/images.md). A host agent never registers an image.

A button for this on the web UI's Add machines page is planned and **not available
yet**: run the command on the host.

## AWS locations

A customer's own AWS account: each machine is one EC2 instance in the customer's
VPC, started by an agent that runs in that account and holds nothing but its
instance role. Forecrew has no credentials for the account. The setup kit
(`deploy/byoc/aws`: CloudFormation and OpenTofu) and the runbook for the cloud
team are in [runbooks/byoc-aws.md](runbooks/byoc-aws.md).

- **One instance per job.** The shape's `provider_config` names `instance_type`,
  and optionally `subnet`, `security_group` and `spot`; the image is an AMI
  (`ami:<region>/<id>`). A fresh EC2 instance boots in about a minute, slower than
  a microVM: say so in the shape's `note`. The disk is an encrypted gp3 volume of
  `disk_gb`. A shape that promises more CPUs, memory or GPUs than its instance type
  has, or another architecture, is refused when the machine is created. Machines are Linux; macOS runs on an EC2 Mac dedicated host through
  Tart, and a metal or nested-virtualization instance can host microVMs (kit modes
  `mac` and `microvm`: the agent's own instance is the host, and the kind is
  `tart` or `microvm`, not `aws`).
- **Tags.** Everything created carries `forecrew:fleet` (the agent's fleet name),
  `forecrew:machine`, `forecrew:org` and `forecrew:job` when known, the
  location's `tags` and the machine's own, so the agent finds its machines by tag,
  and the role of the kit lets it touch nothing without the fleet tag.
- **The image.** Before first use the agent checks the image's signature against
  `trusted_keys_file`, the AMI's architecture, region and state, and that the AMI
  is in the agent's account with the tag `forecrew:image-digest` equal to the
  signed digest (or in an account listed in `trusted_ami_owners`). The signature
  covers the manifest, not the AMI's ID: this is what binds them.
- **Delivery of the token** is cloud-init user data that writes
  `/run/forecrew/bootstrap.env`; the worker spends the token first, as on every
  machine. IMDSv2 only, one hop, no instance profile.
- **Idempotent.** Create terminates what exists for the machine ID and starts
  again; Destroy terminates, and an instance that is gone or ending succeeds.
  Destroying an instance without the fleet tag is refused before EC2 is asked.
- **Limits and cleaning.** `max_machines`, `monthly_budget` (an estimate, from the
  shapes' prices), `max_lifetime` and the sweep of `List` are described in the
  runbook; running out of room, quota or spot capacity is `fleet.ErrCapacity`.
  A spot machine taken back is not listed, so forecrewd fails it at its next
  reconciliation and its job is requeued through its lease.
- **Hosted instances.** With `fleet.hosted: true` (config.md), attaching an own
  cloud to an organization needs its fleet policy's `plan` to be `paid`, a stub
  until billing exists (S.3). Instance-wide locations and other kinds are not
  gated.

### aws.yaml

`/etc/forecrew-fleet-agent/aws.yaml` (`FORECREW_AWS_CONFIG` moves it); the kit
writes it. It holds no credentials and refuses unknown keys, so a pasted access
key is an error: the AWS SDK finds credentials by itself (the instance role, or
`AWS_PROFILE` for a test).

| Key | Default | Meaning |
|-----|---------|---------|
| `region` | required | Where machines start |
| `fleet` | required | The `forecrew:fleet` tag; the role allows exactly this, and the agent terminates every instance in the region with it, so it is unique per account and region |
| `subnets` | required | Subnets machines may use; a shape names one or gets the first |
| `security_group` | required | The machines' group: no inbound, public internet out (kit) |
| `instance_types` | required | Instance types and patterns (`c7i.*`) shapes may use |
| `trusted_keys_file` | `/etc/forecrew-fleet-agent/image-keys.pub` | Public keys images must be signed with |
| `trusted_ami_owners` | none | Accounts whose AMIs need no digest tag; widen the role to match |
| `assign_public_ip` | `false` | `true` for a public subnet |
| `max_machines` | `10` | Machines at once, counted from EC2 by tag |
| `monthly_budget` | `0` | Stops new machines at this estimated spend; 0 is none |
| `budget_warn_percent` | `80` | Where the `budget` check warns |
| `max_lifetime` | `24h` | No machine lives longer |
| `state_dir` | `/var/lib/forecrew-fleet-agent/aws` | The spend ledger |

