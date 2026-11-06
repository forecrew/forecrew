# Forecrew macOS image for Tart: clones a macOS base VM and adds the worker, git,
# common toolchains and the agent CLIs. Runs on an Apple silicon Mac only (the
# macOS licence allows macOS VMs on Apple hardware: docs/images.md, Licensing).
# The result is a local Tart VM, pushed to a registry you control for the fleet
# agent to clone per job. See ../README.md.
packer {
  required_plugins {
    tart = {
      version = ">= 1.21.0"
      source  = "github.com/cirruslabs/tart"
    }
  }
}

variable "version" {
  type        = string
  description = "Image version, for example 2026.11.1."
}

variable "base_vm" {
  type        = string
  description = "Tart VM to start from: a macOS install with the Xcode command line tools and an admin account."
  default     = "ghcr.io/cirruslabs/macos-tahoe-base:latest"
}

variable "ssh_username" {
  type    = string
  default = "admin"
}

variable "ssh_password" {
  type      = string
  sensitive = true
  default   = "admin"
}

variable "worker_binary" {
  type        = string
  description = "A static forecrew-worker for darwin/arm64 (GOOS=darwin GOARCH=arm64 go build ./cmd/forecrew-worker)."
}

variable "cli_package_json" {
  type    = string
  default = "../../worker/control/package.json"
}

variable "cli_package_lock" {
  type    = string
  default = "../../worker/control/package-lock.json"
}

variable "guest_dir" {
  type    = string
  default = "../../../internal/images/guest"
}

variable "output_dir" {
  type    = string
  default = "../out"
}

source "tart-cli" "macos" {
  vm_base_name = var.base_vm
  vm_name      = "forecrew-macos-${var.version}"
  cpu_count    = 4
  memory_gb    = 8
  disk_size_gb = 80
  ssh_username = var.ssh_username
  ssh_password = var.ssh_password
  ssh_timeout  = "300s"
  headless     = true
}

build {
  name    = "macos"
  sources = ["source.tart-cli.macos"]

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
  provisioner "shell" {
    script = "provision-macos.sh"
  }
  provisioner "file" {
    direction   = "download"
    source      = "/etc/forecrew/packages"
    destination = "${var.output_dir}/packages-macos-arm64.txt"
  }
}
