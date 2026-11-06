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

What is in a Linux image: git, Go, Node, Python and a C toolchain from Debian
13; Claude Code, Codex and opencode at the exact versions of
`deploy/worker/control/package.json` and its lock file (one list for the control
worker image and for machine images; bump it there); `forecrew-worker`, started
at boot. Its labels are in `/etc/forecrew/labels` and in the manifest's
`contents.labels`: list the ones a shape needs in the shape.

## Build

Needs Packer with the plugins the templates name (`packer init` fetches them).
Build the worker for the image first (`CGO_ENABLED=0 go build ./cmd/forecrew-worker`
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
  repository. The kernel is a build input, not built here. The rootfs needs
  `mke2fs` 1.47.1 or newer (for example Debian 13): older ones cannot make a
  file system from a tar and keep file owners.
- **Windows and macOS** need licensed hosts, so CI only runs `packer validate` on
  them (`make images-validate`); see Licensing.

## Docker images as machine contents

An environment image (`forecrew-env:<hash>`, [worker.md](worker.md)) or a toolbox
image works unchanged as the contents of a microVM:

```sh
forecrew image rootfs --from forecrew-env:3f9a... --worker ./forecrew-worker --label docker --cache /var/cache/forecrew-images
```

The command exports the image's file system, adds the worker and the guest
files (`/opt/forecrew/`), and makes an ext4 file with `mke2fs`. The result is
cached under the image's content ID, the added files and the size, so the same
environment hash is converted once and every later machine reuses the file. The
image needs `/bin/sh`; device nodes are dropped (the guest mounts `devtmpfs`),
paths that leave the root are refused, and size and file count are bounded. The
file system is sized from the content plus room to write; a provider resizes the
disk copy to the shape's `disk_gb`.

## Inside the guest

`internal/images/guest` is the one copy of what runs in a machine, used by the
converter and the templates alike.

- `forecrew-init` is PID 1 of a microVM (`init=/opt/forecrew/forecrew-init`). It
  mounts the pseudo file systems, brings up the network from the kernel command
  line (`forecrew.ip=`, `forecrew.gw=`, `forecrew.dns=`), copies `bootstrap.env`
  from the **config disk** (`forecrew.config=`, default `/dev/vdb`, ext4 or
  vfat, read-only) into RAM, runs the worker and powers the machine off when it
  ends.
- `forecrew-start` (Linux, macOS) and `forecrew-start.ps1` (Windows) read the
  settings the provider delivered, write `worker.yaml` and run the worker with
  only its token in the environment. The settings file is read as data, never as
  shell, with a fixed set of keys and a restricted character set, and deleted
  once read. It never reaches a disk of the host: the provider delivers it by
  cloud-init, a config disk, `tart exec` or Hyper-V, into a RAM disk.

| Setting | Meaning |
|---------|---------|
| `FORECREW_SERVER_URL` | forecrewd's URL; required |
| `FORECREW_BOOTSTRAP_TOKEN` or `FORECREW_WORKER_TOKEN` | the machine's one-time `fcb_` token, or a worker token; exactly one |
| `FORECREW_WORKER_NAME`, `FORECREW_WORKER_LABELS`, `FORECREW_WORKER_MAX_JOBS` | optional; the labels add to the image's |

Where the file goes: `/run/forecrew/bootstrap.env` (Linux),
`/var/run/forecrew/bootstrap.env` (macOS), `C:\ProgramData\Forecrew\run\bootstrap.env`
(Windows). On systemd images `forecrew-worker.service` starts when the file
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

**Registering** is for a person: `forecrew image add <manifest>`
(`POST /api/v1/images`) needs a person's credentials and `worker.manage`; an
instance-wide image (`--instance`) also needs an instance admin. A service
account, an ownerless token or a machine is refused, and there is no MCP tool
for it, so an agent can never put an image in the table that others trust. CI
may build and publish files; a person signs and registers. `forecrew image list`
and the MCP tool `list_images` show what is registered.

## Versions and retention

Images are versioned: a shape names `name` for the newest build or `name@version`
to pin one. `forecrew image prune [--keep 3] [--dry-run] [--instance]` is the
retention rule: of each image (same owner, name, OS, architecture and format) the
newest builds stay and the rest are removed. An image a live machine's shape
pins is kept (`409`), so a running fleet is never pulled from under. Prune from
a schedule if you build often.

## Licensing

Forecrew does not distribute Windows or macOS. The Windows template builds on
your host from an ISO you supply and refuses to run until you set
`license_confirmed=true`; the result is an unsigned manifest that you check,
sign with your own key and register under your organization, and only fleet
agents configured with your key will use it. The macOS template needs an Apple
silicon Mac, as the macOS licence allows macOS VMs on Apple hardware only. The
terms of both, and the 24-hour minimum of cloud Macs, are to be rechecked before
selling machines (decision 29 of the plan).

## Wiring the resolver

`images.Resolver(store.Images())` is the function the fleet controller takes as
`fleet.Config.Image`: it finds what a shape's reference names for the
organization, in a format the provider starts.
