# Forecrew Windows Server image, built on a host you own from an ISO you supply.
# Forecrew cannot distribute Windows (docs/images.md, Licensing): this template
# refuses to run until license_confirmed is set, and leaves an UNSIGNED manifest
# that a person checks, signs and registers. Needs a Linux host with KVM and
# QEMU, or adapt the source for Hyper-V. See ../README.md.
packer {
  required_plugins {
    qemu = {
      version = ">= 1.1.8"
      source  = "github.com/hashicorp/qemu"
    }
  }
}

variable "version" {
  type        = string
  description = "Image version, for example 2026.11.1."
}

variable "iso_path" {
  type        = string
  description = "Path of a Windows Server ISO you are licensed to use. Forecrew does not provide one."
}

variable "iso_checksum" {
  type        = string
  description = "Checksum of the ISO, for example sha256:<hex>, from your licence portal."
}

variable "license_confirmed" {
  type        = bool
  default     = false
  description = "Set to true to confirm that you hold a licence for the ISO and for running it as a build machine."
  validation {
    condition     = var.license_confirmed
    error_message = "Set license_confirmed=true to confirm that you hold a Windows Server licence that covers this use (docs/images.md, Licensing)."
  }
}

variable "windows_image_name" {
  type        = string
  default     = "Windows Server 2022 SERVERSTANDARD"
  description = "The edition inside the ISO (the /IMAGE/NAME in install.wim)."
}

variable "worker_binary" {
  type        = string
  description = "A forecrew-worker.exe for windows/amd64 (GOOS=windows go build ./cmd/forecrew-worker)."
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

variable "accelerator" {
  type    = string
  default = "kvm"
}

locals {
  # Used by the build only; the account's password is replaced before shutdown.
  admin_password = uuidv4()
}

source "qemu" "windows" {
  iso_url          = var.iso_path
  iso_checksum     = var.iso_checksum
  format           = "qcow2"
  disk_size        = "60G"
  accelerator      = var.accelerator
  headless         = true
  memory           = 4096
  cpus             = 2
  # IDE and e1000 need no extra drivers; boot time is not the point of a build.
  disk_interface   = "ide"
  net_device       = "e1000"
  boot_wait        = "2s"
  boot_command     = ["<space><wait><space><wait><space>"]
  cd_content = {
    "Autounattend.xml" = templatefile("Autounattend.xml.pkrtpl.hcl", {
      password   = local.admin_password
      image_name = var.windows_image_name
    })
  }
  cd_label         = "UNATTEND"
  communicator     = "winrm"
  winrm_username   = "Administrator"
  winrm_password   = local.admin_password
  winrm_timeout    = "2h"
  # finish.ps1 (written by provision-windows.ps1) removes the build's WinRM access.
  shutdown_command = "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\ProgramData\\Forecrew\\finish.ps1"
  output_directory = "${var.output_dir}/windows-qcow2"
  vm_name          = "forecrew-windows-${var.version}-amd64.qcow2"
}

build {
  name    = "windows"
  sources = ["source.qemu.windows"]

  provisioner "powershell" {
    inline = ["New-Item -ItemType Directory -Force -Path C:\\ProgramData\\Forecrew\\run, C:\\ProgramData\\Forecrew\\cli | Out-Null"]
  }
  provisioner "file" {
    source      = var.worker_binary
    destination = "C:\\ProgramData\\Forecrew\\forecrew-worker.exe"
  }
  provisioner "file" {
    source      = "${var.guest_dir}/forecrew-start.ps1"
    destination = "C:\\ProgramData\\Forecrew\\forecrew-start.ps1"
  }
  provisioner "file" {
    sources     = [var.cli_package_json, var.cli_package_lock]
    destination = "C:\\ProgramData\\Forecrew\\cli\\"
  }
  provisioner "powershell" {
    script = "provision-windows.ps1"
  }
  provisioner "file" {
    direction   = "download"
    source      = "C:\\ProgramData\\Forecrew\\packages"
    destination = "${var.output_dir}/packages-windows-amd64.txt"
  }

  # Windows Server on Hyper-V wants a vhdx; the qcow2 stays for KVM hosts.
  post-processor "shell-local" {
    inline = [
      "qemu-img convert -O vhdx -o subformat=dynamic ${var.output_dir}/windows-qcow2/forecrew-windows-${var.version}-amd64.qcow2 ${var.output_dir}/windows-qcow2/forecrew-windows-${var.version}-amd64.vhdx",
    ]
  }
}
