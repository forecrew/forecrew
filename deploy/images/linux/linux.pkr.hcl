# Forecrew Linux machine images (docs/images.md): a qcow2 disk for VMs
# (Proxmox, cloud) and a container whose export becomes the ext4 root
# filesystem of a microVM. Run from this directory; see ../README.md and
# ../build.sh, which also writes and signs the manifest.
packer {
  required_plugins {
    qemu = {
      version = "= 1.1.8"
      source  = "github.com/hashicorp/qemu"
    }
    docker = {
      version = "= 1.1.5"
      source  = "github.com/hashicorp/docker"
    }
  }
}

variable "version" {
  type        = string
  description = "Image version, for example 2026.11.1."
}

variable "arch" {
  type    = string
  default = "amd64"
  validation {
    condition     = contains(["amd64", "arm64"], var.arch)
    error_message = "The arch variable must be amd64 or arm64."
  }
}

variable "worker_binary" {
  type        = string
  description = "A static forecrew-worker for linux and the image's arch (CGO_ENABLED=0 go build ./cmd/forecrew-worker)."
}

variable "base_image_url" {
  type        = string
  description = "Debian generic cloud image the qcow2 is built on. Empty: the dated release pinned in locals, for var.arch."
  default     = ""
}

variable "base_image_checksum" {
  type        = string
  description = "sha512:<hex> of base_image_url. Empty: the pinned one. Always give it with another URL."
  default     = ""
}

variable "container_image" {
  type        = string
  description = "Base image of the microVM root filesystem, by digest. Empty: the digest pinned in locals."
  default     = ""
}

variable "uefi_firmware" {
  type        = string
  description = "arm64 only: path of the UEFI firmware (AAVMF) the VM boots, for example /usr/share/AAVMF/AAVMF_CODE.fd."
  default     = ""
}

variable "cli_package_json" {
  type        = string
  description = "Pins the agent CLIs (Claude Code, Codex, opencode); shared with the control worker image."
  default     = "../../worker/control/package.json"
}

variable "cli_package_lock" {
  type    = string
  default = "../../worker/control/package-lock.json"
}

variable "guest_dir" {
  type        = string
  description = "The guest files that run inside every machine (internal/images/guest)."
  default     = "../../../internal/images/guest"
}

variable "output_dir" {
  type    = string
  default = "../out"
}

variable "accelerator" {
  type    = string
  default = "kvm"
}

variable "qemu_binary" {
  type        = string
  description = "Empty: qemu-system-x86_64 for amd64, qemu-system-aarch64 for arm64."
  default     = ""
}

locals {
  # Pinned: a dated Debian cloud image with its checksum (from the release's
  # SHA512SUMS), and the debian:trixie-slim index digest. Bump them together,
  # on purpose: a moving "latest" would make two builds of one version differ.
  debian_release = "20261103-2651"
  debian_checksums = {
    amd64 = "sha512:1207e6e3efaff8ee77ab7a32c156a3eea6a9e53f94cfc48efbd1f49ba554ca36ac62ab27395aa813a1775a8c0a393b1a1ce5377d9e5670f6113e3110c57fee4b"
    arm64 = "sha512:878f563f6c95af7fe1e8e51cd39470a1cdb570dddbee22377c85a347bce9d521d95f2cf0f825a38bc0a757889034fac6d83e932f04a3a122ab08cea68cc9ff8a"
  }
  base_image_url      = coalesce(var.base_image_url, "https://cloud.debian.org/images/cloud/trixie/${local.debian_release}/debian-13-generic-${var.arch}-${local.debian_release}.qcow2")
  base_image_checksum = coalesce(var.base_image_checksum, local.debian_checksums[var.arch])
  container_image     = coalesce(var.container_image, "debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2")
  qemu_binary         = coalesce(var.qemu_binary, var.arch == "amd64" ? "qemu-system-x86_64" : "qemu-system-aarch64")
  machine_type        = var.arch == "amd64" ? "pc" : "virt"
  qemuargs            = var.arch == "arm64" ? [["-bios", var.uefi_firmware], ["-cpu", "host"]] : []

  # Cloud-init NoCloud seed for the build VM only; provision-linux.sh locks
  # the account and resets cloud-init before the image is kept.
  user_data = <<-EOT
    #cloud-config
    ssh_pwauth: true
    users:
      - name: packer
        sudo: ALL=(ALL) NOPASSWD:ALL
        shell: /bin/bash
        lock_passwd: false
        plain_text_passwd: packer
  EOT
}

source "qemu" "qcow2" {
  iso_url          = local.base_image_url
  iso_checksum     = local.base_image_checksum
  disk_image       = true
  format           = "qcow2"
  disk_size        = "20G"
  accelerator      = var.accelerator
  qemu_binary      = local.qemu_binary
  machine_type     = local.machine_type
  qemuargs         = local.qemuargs
  headless         = true
  memory           = 4096
  cpus             = 2
  cd_label         = "cidata"
  cd_content = {
    "user-data" = local.user_data
    "meta-data" = "instance-id: forecrew-image-build\n"
  }
  ssh_username     = "packer"
  ssh_password     = "packer"
  ssh_timeout      = "20m"
  shutdown_command = "sudo sh -c 'rm -f /etc/sudoers.d/90-cloud-init-users; passwd -l packer; shutdown -P now'"
  output_directory = "${var.output_dir}/linux-qcow2-${var.arch}"
  vm_name          = "forecrew-linux-${var.version}-${var.arch}.qcow2"
}

source "docker" "rootfs" {
  image       = local.container_image
  platform    = "linux/${var.arch}"
  export_path = "${var.output_dir}/linux-rootfs-${var.arch}.tar"
}

build {
  name    = "linux"
  sources = ["source.qemu.qcow2", "source.docker.rootfs"]

  provisioner "shell" {
    inline = ["mkdir -p /tmp/forecrew-guest"]
  }
  provisioner "file" {
    source      = "${var.guest_dir}/"
    destination = "/tmp/forecrew-guest"
  }
  provisioner "file" {
    source      = var.worker_binary
    destination = "/tmp/forecrew-worker"
  }
  provisioner "file" {
    sources     = [var.cli_package_json, var.cli_package_lock]
    destination = "/tmp/"
  }

  # A VM has sudo and systemd; the container is root and has neither.
  provisioner "shell" {
    only             = ["qemu.qcow2"]
    script           = "provision-linux.sh"
    environment_vars = ["FORECREW_IMAGE_KIND=vm"]
    execute_command  = "{{ .Vars }} sudo -E sh '{{ .Path }}'"
  }
  provisioner "shell" {
    only             = ["docker.rootfs"]
    script           = "provision-linux.sh"
    environment_vars = ["FORECREW_IMAGE_KIND=container"]
    execute_command  = "{{ .Vars }} sh '{{ .Path }}'"
  }

  # What the image holds, for the manifest.
  provisioner "file" {
    direction   = "download"
    source      = "/etc/forecrew/packages"
    destination = "${var.output_dir}/packages-${source.name}-${var.arch}.txt"
  }
}
