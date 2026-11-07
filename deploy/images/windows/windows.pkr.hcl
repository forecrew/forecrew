# Forecrew Windows Server image, built on a host you own from an ISO you supply.
# Forecrew cannot distribute Windows (docs/images.md, Licensing): this template
# refuses to run until license_confirmed is set, and leaves an UNSIGNED manifest
# that a person checks, signs and registers. Two sources, picked with -only:
# qemu.windows on a Linux host with KVM and QEMU (also the host of a Proxmox
# location), and hyperv-iso.windows on a Windows host with Hyper-V. See
# ../README.md; the fleet agent's build-image command runs this template.
packer {
  required_plugins {
    qemu = {
      version = "= 1.1.8"
      source  = "github.com/hashicorp/qemu"
    }
    hyperv = {
      version = "= 1.1.5"
      source  = "github.com/hashicorp/hyperv"
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
  description = "Checksum of the ISO, for example sha256:<hex>, from your licence portal. Required: there is no default, and none is refused."
  validation {
    condition     = can(regex("^(sha256|sha512|sha1|md5):[0-9a-fA-F]+$", var.iso_checksum))
    error_message = "The iso_checksum variable must be <algorithm>:<hex>, for example sha256:<hex>; none is not accepted."
  }
}

variable "license_confirmed" {
  type        = bool
  default     = false
  description = "Set to true to confirm that you hold a licence for the ISO and for running it as a build machine."
  validation {
    condition     = var.license_confirmed
    error_message = "Set license_confirmed to true to confirm that you hold a Windows Server licence that covers this use."
  }
}

variable "chocolatey_installer_sha256" {
  type        = string
  description = "SHA-256 of the Chocolatey install.ps1 you reviewed (Get-FileHash). The script runs only if its hash matches; the Chocolatey version is pinned in provision-windows.ps1."
  validation {
    condition     = can(regex("^[0-9a-fA-F]{64}$", var.chocolatey_installer_sha256))
    error_message = "The chocolatey_installer_sha256 variable must be 64 hex digits."
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

variable "hyperv_switch" {
  type        = string
  default     = "Default Switch"
  description = "The virtual switch of the build VM on Hyper-V; it needs a route to the internet, for Chocolatey and npm."
}

variable "guest_tools_installer" {
  type        = string
  default     = ""
  description = "Path of virtio-win-guest-tools.exe, for an image a Proxmox location starts: it holds the QEMU guest agent the provider delivers the bootstrap settings through, and the virtio drivers. Not needed on Hyper-V. Forecrew does not download it: take it from a source you trust."
}

variable "guest_tools_sha256" {
  type        = string
  default     = ""
  description = "SHA-256 of that installer; it is installed only if the hash matches."
  validation {
    condition     = var.guest_tools_sha256 == "" || can(regex("^[0-9a-fA-F]{64}$", var.guest_tools_sha256))
    error_message = "The guest_tools_sha256 variable must be 64 hex digits, or empty."
  }
}

variable "accelerator" {
  type    = string
  default = "kvm"
}

# Used by the build only. Sysprep generalizes the image and the clones' own
# first boot gives the account another random password and disables it, so
# machines share neither this password nor a SID.
local "admin_password" {
  expression = uuidv4()
  sensitive  = true
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
  # finish.ps1 (written by provision-windows.ps1) turns WinRM off and runs
  # sysprep /generalize /shutdown, which takes a while.
  shutdown_timeout = "45m"
  shutdown_command = "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\ProgramData\\Forecrew\\finish.ps1"
  output_directory = "${var.output_dir}/windows-qcow2"
  vm_name          = "forecrew-windows-${var.version}-amd64.qcow2"
}

# The same machine on Hyper-V: BIOS (generation 1) like the QEMU build, so the
# answer file and the image are the same. The Autounattend.xml goes on a floppy,
# which generation 1 has. Not run by Forecrew's CI: no Windows host.
source "hyperv-iso" "windows" {
  iso_url          = var.iso_path
  iso_checksum     = var.iso_checksum
  generation       = 1
  switch_name      = var.hyperv_switch
  cpus             = 2
  memory           = 4096 # MB, as is disk_size
  disk_size        = 61440
  boot_wait        = "2s"
  boot_command     = ["<space><wait><space><wait><space>"]
  floppy_content = {
    "Autounattend.xml" = templatefile("Autounattend.xml.pkrtpl.hcl", {
      password   = local.admin_password
      image_name = var.windows_image_name
    })
  }
  communicator     = "winrm"
  winrm_username   = "Administrator"
  winrm_password   = local.admin_password
  winrm_timeout    = "2h"
  shutdown_timeout = "45m"
  shutdown_command = "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\ProgramData\\Forecrew\\finish.ps1"
  output_directory = "${var.output_dir}/windows-hyperv"
  vm_name          = "forecrew-windows-${var.version}-amd64"
}

build {
  name    = "windows"
  sources = ["source.qemu.windows", "source.hyperv-iso.windows"]

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
  provisioner "file" {
    source      = "sysprep.xml"
    destination = "C:\\ProgramData\\Forecrew\\sysprep.xml"
  }
  # Always uploaded; without an installer it is a note, and nothing is installed.
  provisioner "file" {
    source      = var.guest_tools_installer != "" ? var.guest_tools_installer : "no-guest-tools.txt"
    destination = "C:\\ProgramData\\Forecrew\\guest-tools.exe"
  }
  provisioner "powershell" {
    script = "provision-windows.ps1"
    environment_vars = [
      "CHOCOLATEY_INSTALLER_SHA256=${var.chocolatey_installer_sha256}",
      "GUEST_TOOLS=${var.guest_tools_installer != "" ? "1" : "0"}",
      "GUEST_TOOLS_SHA256=${var.guest_tools_sha256}",
    ]
  }
  provisioner "file" {
    direction   = "download"
    source      = "C:\\ProgramData\\Forecrew\\packages"
    destination = "${var.output_dir}/packages-windows-amd64.txt"
  }

  # Windows Server on Hyper-V wants a vhdx; the qcow2 stays for KVM hosts.
  post-processor "shell-local" {
    only = ["qemu.windows"]
    inline = [
      "qemu-img convert -O vhdx -o subformat=dynamic ${var.output_dir}/windows-qcow2/forecrew-windows-${var.version}-amd64.qcow2 ${var.output_dir}/windows-qcow2/forecrew-windows-${var.version}-amd64.vhdx",
    ]
  }
}
