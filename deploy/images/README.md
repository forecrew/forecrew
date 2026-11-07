# Machine images

Packer templates for the images the fleet starts machines from, with
`build.sh` for the Linux ones and `validate.sh` for CI. The full guide is
[docs/images.md](../../docs/images.md): what is in an image, how Docker images
become microVM root filesystems, signing, registering, retention and licensing.

| Directory | Builds | Host |
|-----------|--------|------|
| `linux/` | `qcow2` (QEMU) and the container for a microVM `rootfs` | Linux with KVM and Docker |
| `windows/` | Windows Server `qcow2` and `vhdx` from your ISO | Linux with KVM; set `license_confirmed=true` |
| `macos/` | a Tart VM | Apple silicon Mac |

```sh
make build                                   # forecrew and forecrew-worker in bin/
CGO_ENABLED=0 go build -o forecrew-worker ./cmd/forecrew-worker
FORECREW=bin/forecrew deploy/images/build.sh qcow2 --version 2026.11.1 --worker ./forecrew-worker --uri-base https://images.example.com
make images-validate                         # packer validate of every template
# a host whose mke2fs is older than 1.47.1 or lacks libarchive13t64: MKE2FS=deploy/images/mke2fs-docker.sh ... build.sh rootfs
```

Windows, with your own ISO:

```sh
cd deploy/images/windows && packer init .
packer build -var version=2026.11.1 -var worker_binary=./forecrew-worker.exe \
  -var iso_path=/isos/server2022.iso -var iso_checksum=sha256:<hex> -var license_confirmed=true \
  -var chocolatey_installer_sha256=<hash of the install.ps1 you reviewed> .
forecrew image manifest --name forecrew-windows --version 2026.11.1 --os windows --format qcow2 \
  --file ../out/windows-qcow2/forecrew-windows-2026.11.1-amd64.qcow2 --uri https://images.example.com/forecrew-windows-2026.11.1.qcow2
forecrew image sign ... --key image.key && forecrew image add ...   # a person checks, signs and registers it
```

macOS, on the Mac: `cd deploy/images/macos && packer init . && packer build -var version=2026.11.1 -var worker_binary=./forecrew-worker -var base_vm=ghcr.io/cirruslabs/<base>@sha256:<hex> .`
then push the Tart VM to your registry and write a manifest with `--format tart --uri oci://<registry>/<name>:<tag> --digest sha256:<hex of the build's manifest> --label xcode:<major>` (the label only when the base VM has Xcode). The fleet agent pulls it by that digest, never by the tag.
