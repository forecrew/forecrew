# Machine images

The fleet ([fleet.md](contracts/fleet.md)) starts a machine for a job from an
**image**: an OS with `forecrew-worker`, git, the agent CLIs and common
toolchains. This page is how images are built, converted, signed, registered
and removed. Starting machines from them is the fleet agent's job.

| Image | Formats | Built by | Used by |
|-------|---------|----------|---------|
| Linux, microVM | `rootfs` (ext4) plus a kernel | `deploy/images/linux` (the container source) and `forecrew image rootfs` | Firecracker, Cloud Hypervisor |
| Linux, VM | `qcow2` | `deploy/images/linux` (the QEMU source) | Proxmox, Hyper-V (converted), cloud |
| Windows Server | `vhdx`, `qcow2` | `deploy/images/windows`, on a host, from your ISO | Hyper-V, Proxmox |
| macOS | `tart` | `deploy/images/macos`, on an Apple silicon Mac | Tart |

What is in a Linux image: git, Go, Python and a C toolchain from Debian 13, and Node
24.22.2 from nodejs.org with its checksum pinned (Claude Code needs Node 22 or newer); Claude Code, Codex and opencode at the exact versions of
`deploy/worker/control/package.json` and its lock file (one list for the control
worker image and for machine images; bump it there); `forecrew-worker`, started
at boot. Its labels are in `/etc/forecrew/labels` and in the manifest's
`contents.labels`: list the ones a shape needs in the shape.

## Build

Needs Packer with the plugins the templates pin exactly (`packer init` fetches
them). The inputs are pinned too: a dated Debian cloud image with its checksum,
the `debian:trixie-slim` index digest, the Chocolatey and macOS base VM
versions; bump them together, on purpose. Build the worker for the image first (`CGO_ENABLED=0 go build ./cmd/forecrew-worker`
with the image's `GOOS` and `GOARCH`).

```sh
deploy/images/build.sh qcow2 --version 2026.11.1 --worker ./forecrew-worker --uri-base https://images.example.com
deploy/images/build.sh rootfs --version 2026.11.1 --worker ./forecrew-worker --uri-base https://images.example.com --kernel ./vmlinux
```

Output goes to `deploy/images/out`: the image file and a **manifest**
(`*.manifest.json`, `$defs/image` of `fleet.schema.json`) with the digest, the
labels, the packages and the build time. Customers build the same templates in
their own account; nothing in them is specific to Forecrew's infrastructure.

- **microVM kernel.** A `rootfs` image comes with the kernel it boots
  (`contents.kernel`). Use a kernel with virtio block, virtio net and ext4 built
  in; Firecracker publishes one, and its build configs are in the Firecracker
  repository. The kernel is a build input, not built here. Its digest and the
  build time are signed with the image, and the fleet agent checks the kernel
  file against it (`images.VerifyFile`, which takes the image file and its kernel file together), so a kernel cannot be swapped under a
  signed manifest.
- **mke2fs.** The rootfs needs `mke2fs` 1.47.1 or newer **with libarchive**
  (Debian 13: `apt-get install e2fsprogs libarchive13t64`): older ones cannot make
  a file system from a tar and keep file owners, and without libarchive `mke2fs -d`
  cannot read the tar. `forecrew image rootfs` tries a small tar first and says
  which is missing. Before 1.47.4 `-d` fails on files of 2 GiB and more, so the
  converter refuses an image that has one. On a host with an older `mke2fs` use
  `deploy/images/mke2fs-docker.sh`, which runs a Debian 13 one in a container
  (`MKE2FS=deploy/images/mke2fs-docker.sh deploy/images/build.sh rootfs ...`).
- **CI** (`.github/workflows/images.yml`) validates every template and builds the
  Linux qcow2 on each change; the artifacts are the image and an unsigned
  manifest, since CI does not sign or register. The rootfs and the boot test
  need a kernel: **the repository variables `FORECREW_KERNEL_URL` and
  `FORECREW_KERNEL_SHA256` are the owner's to set**, to a kernel they trust; the
  two jobs are skipped until they are.
- **Windows and macOS** need licensed hosts, so CI only runs `packer validate` on
  them (`make images-validate`); see Licensing. The Tart plugin ships darwin binaries only, so off macOS the macOS template is checked for syntax (`packer validate -syntax-only`), and fully on a Mac.

## What an image needs

A Linux image, however it is made, must hold what the worker and the init use;
`forecrew image rootfs` checks it and lists everything missing at once. Commands
are looked for in the usual directories (`/bin`, `/usr/bin`, `/usr/local/bin`, ...)
and in the directories of the image's own `PATH`, so an image that keeps its CLIs
elsewhere, like the toolbox base image's `/opt/forecrew-cli/node_modules/.bin`,
passes.

| Needs | Why |
|-------|-----|
| `sh` | the worker and jobs run commands with it; `forecrew-start` of VM images is a POSIX shell script |
| `git` | the worker clones and commits with it |
| CA certificates | the worker and git speak HTTPS |
| `claude`, `codex`, `opencode` | the agent CLIs of agent jobs; `--no-agent-clis` for an image of command jobs only |

The image's **environment** (a Docker image's `Config.Env`: `PATH` to its
toolchains, `GOFLAGS`, ...) is saved to `/etc/forecrew/env` and applied to the
worker as data (names are checked, `FORECREW_*` is reserved, nothing is run as
shell); it is part of the cache key.

## Docker images as machine contents

An environment image (`forecrew-env:<hash>`, [worker.md](worker.md)) or a toolbox
image works unchanged as the contents of a microVM:

```sh
forecrew image rootfs --from forecrew-env:3f9a... --worker ./forecrew-worker --label docker --cache /var/cache/forecrew-images
```

The command exports the image's file system by its content ID (so a tag that
moves meanwhile cannot change what is converted), adds the worker and the guest
files (`/opt/forecrew/`, which is Forecrew's: whatever the image has there is
dropped, and an `/opt` that is not a directory is refused) and the image's environment, and makes an ext4 file with
`mke2fs`. The result is cached under the image's content ID, the added files and
the size, so the same environment hash is converted once and every later machine
reuses the file. Device nodes are dropped (the guest mounts `devtmpfs`), paths
that leave the root are refused, and size and file count are bounded. The file
system is sized from the content in whole blocks per file plus an inode per
entry, the journal and room to write; a provider resizes the disk copy to the
shape's `disk_gb`.

## Inside the guest

`internal/images/guest` is the one copy of what runs in a machine, used by the
converter and the templates alike.

- **PID 1 of a microVM is `forecrew-worker guest-init`**, started by the kernel
  (`init=/opt/forecrew/forecrew-worker -- guest-init`). It is a static program
  on purpose: a root disk converted from a repo's Dockerfile holds the repo's
  `/bin/sh`, `git`, CA bundle, `/etc/hosts` and proxy settings, so **nothing of
  the image runs, and nothing the image says is used, until the machine's
  bootstrap token is spent**. The init mounts the pseudo file systems and
  `cgroup2`, loads the kernel modules the command line names
  (`forecrew.modules=`, never a list of the image's), brings up the network over
  netlink (`forecrew.ip=`, `forecrew.gw=`, `forecrew.dns=`), grows the root file
  system to its disk (`forecrew.root=`), mounts the read-only cache disk
  (`forecrew.cache=`, at `/var/cache/forecrew`), and reads the **config disk**
  (`forecrew.config=`, default `/dev/vdb`). That disk holds the settings and the
  token, the server's addresses as the host resolved them and, for https, the CA
  bundle of the host; the init **wipes it** once read. It then trades the token
  for a worker token itself, dialling only those addresses, with no proxy and
  only that CA. Only then does it start the image's `dockerd`, if there is one,
  and the worker, with the worker token, the image's environment and labels
  read as data, and `worker.yaml` in `/run`. When the worker ends the machine
  ends (`forecrew.halt=reboot` for Firecracker, `poweroff` for Cloud
  Hypervisor). A refused token ends it before anything of the image runs.
  What runs after the exchange runs as root in the guest with the worker, as
  every job does: it can reach the worker token (workers are not a boundary
  from their own user), but the bootstrap token is spent.
- `forecrew-start` (Linux, macOS) and `forecrew-start.ps1` (Windows) read the
  settings the provider delivered, write `worker.yaml` and run the worker with
  only its token and the image's environment. The settings file is read as data,
  never as shell, with a fixed set of keys and a restricted character set, and
  deleted once read.

| Setting | Meaning |
|---------|---------|
| `FORECREW_SERVER_URL` | forecrewd's URL; required |
| `FORECREW_BOOTSTRAP_TOKEN` or `FORECREW_WORKER_TOKEN` | the machine's one-time `fcb_` token, or a worker token; exactly one |
| `FORECREW_WORKER_NAME`, `FORECREW_WORKER_LABELS`, `FORECREW_WORKER_MAX_JOBS` | optional; the labels add to the image's |

Where the file goes, and what that means for the token:

| OS | Path | Storage | Delivery the provider must use |
|----|------|---------|--------------------------------|
| Linux microVM | none: the init reads the config disk into memory | the config disk, wiped by the guest once read, and removed with the machine | a config disk made for this machine (`guestinit.Encode`) |
| Linux VM (systemd) | `/run/forecrew/bootstrap.env` | RAM, but see below | a config drive the guest copies into `/run`; cloud-init `write_files` works but **cloud-init keeps its user-data, token included, on the machine's disk** (`/var/lib/cloud`) until the machine is destroyed |
| macOS (Tart) | `/var/run/forecrew/bootstrap.env` | **the machine's own disk**, mode 0700 root | `tart exec` into the running machine after it is assigned |
| Windows | `C:\ProgramData\Forecrew\run\bootstrap.env` | **the machine's own disk**, ACL for SYSTEM and administrators only | into the running machine after it is assigned: Hyper-V PowerShell Direct. WinRM is turned off in the image, so it is not a route. A Proxmox clone needs the QEMU guest agent or a config disk the guest copies the file from, which T62 must provide |

RAM holds the token on Linux only, and on a systemd image delivered by cloud-init not even there. On macOS and Windows it is a file on the
guest's disk until `forecrew-start` reads and deletes it, so T62 (Windows) and
T63 (macOS) must deliver it into the running machine, never bake it into the
image, a shared folder or a host path, and must destroy the disk with the
machine (fleet.md: it is never written to the host's disk outside the guest's
own image). On systemd images `forecrew-worker.service` starts when the file
exists and powers the machine off when the worker ends.

## Signing and trust

An image is signed by a person with an Ed25519 key kept off the hosts that run
jobs. The signature covers the name, version, OS, architecture, format and
digest, so it fits one image only.

```sh
forecrew image keygen --out image.key --public image.pub     # prints the public key line
forecrew image sign out/forecrew-linux-2026.11.1-amd64.qcow2.manifest.json --key image.key
forecrew image verify out/...manifest.json --trusted image.pub --file out/...qcow2
```

`build.sh --key image.key` signs in one step. The fleet agent checks the
signature and the file's digest (`images.VerifyFile`) before it first uses an
image, against the public keys in its own local configuration, and refuses the
machine otherwise: a tampered file or a manifest signed by an unknown key does
not boot. forecrewd checks the form only.

A signature also covers the kernel's digest on a `rootfs` image and the build
time to the second, so an organization admin cannot swap the kernel under, or
re-date, a signed instance manifest.

**Registering** is for a person: `forecrew image add <manifest>`
(`POST /api/v1/images`) needs a person's credentials and `worker.manage`; an
instance-wide image (`--instance`) also needs an instance admin and must be a
Linux image: Windows and macOS images are never instance-wide. A service
account, an ownerless token or a machine is refused, and there is no MCP tool
for it, so an agent can never put an image in the table that others trust. CI
may build and publish files; a person signs and registers. `forecrew image list`
and the MCP tool `list_images` show what is registered.

## Versions and retention

Images are versioned: a shape names `name` for the newest build or `name@version`
to pin one. `forecrew image prune [--keep 3] [--dry-run] [--instance]` is the
retention rule: of each image (same owner, name, OS, architecture and format) the
newest builds stay and the rest are removed. An image a live machine's shape
pins is kept (`409`): for every live machine, the image `Resolve` gives its
shape's reference, for the machine's organization and the formats its provider
starts, is in use. So a running fleet is never pulled from under. A scheduled
prune means a person's token on a cron job (`forecrew image prune` calls the
API, and deleting an image is for a person, not a service account); forecrewd
does not prune by itself.

Files on a machine are not images of the server. `forecrew image prune-cache
[--cache DIR] [--days 30] [--dry-run]` removes the root filesystems `forecrew
image rootfs` converted and has not used (converted or found in the cache) for
that many days, and what an interrupted conversion left. A fleet agent cleans
its own cache of fetched images by the same rule (docs/fleet.md).

## Windows and macOS builds

- **Windows** is built on your host from an ISO you supply; the template refuses to
  run until you set `license_confirmed=true`, and takes the SHA-256 of the
  Chocolatey `install.ps1` you reviewed (`chocolatey_installer_sha256`): the script
  runs only if its hash matches, at a pinned Chocolatey version. The build's
  Administrator password is random, per build and sensitive. The image is
  **generalized with sysprep**, so every clone has its own SID, WinRM is turned off
  before it, and the answer file (`sysprep.xml`) gives each clone's Administrator a
  random password at its first boot and disables the account; the worker runs as
  SYSTEM and needs no login. Clones share neither a password nor a SID.
- **macOS** needs an Apple silicon Mac. The base VM is pinned by digest
  (`base_vm=registry/name@sha256:...`, which you resolve and review). The base
  VM's login (`admin`/`admin` in the public Cirrus images) is replaced by a random
  password nobody keeps and remote login is turned off at the end of the build, so
  the provider reaches the machine with `tart exec`, not ssh.

## Licensing

Forecrew does not distribute Windows or macOS. The Windows result is an unsigned
manifest that you check, sign with your own key and register under your
organization, and only fleet agents configured with your key will use it. The
macOS licence allows macOS VMs on Apple hardware only. The
terms of both, and the 24-hour minimum of cloud Macs, are to be rechecked before
selling machines (decision 29 of the plan).

## The resolver

forecrewd gives the fleet controller `images.Resolver(store.Images())` as
`fleet.Config.Image`: it finds what a shape's reference names for the
organization, in a format the provider starts.
