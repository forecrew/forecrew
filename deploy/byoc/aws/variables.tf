# The same inputs as cloudformation.yaml. The join code is the only secret.

variable "mode" {
  type        = string
  default     = "ec2"
  description = "ec2: one instance per job, started by the agent. microvm: the agent's own instance is a metal or nested-virtualization host and runs the jobs as microVMs. mac: an EC2 Mac on a dedicated host running Tart VMs (a dedicated host is billed for at least 24 hours)."
  validation {
    condition     = contains(["ec2", "microvm", "mac"], var.mode)
    error_message = "mode is ec2, microvm or mac."
  }
}

variable "server_url" {
  type        = string
  description = "Your forecrewd's public URL."
  validation {
    condition     = can(regex("^https?://[A-Za-z0-9._:/-]+$", var.server_url))
    error_message = "server_url must be an http(s) URL with plain characters."
  }
}

variable "join_code" {
  type        = string
  sensitive   = true
  description = "From forecrew provider join --cloud. It works once and gives no access until a person confirms what the agent reports."
  validation {
    condition     = can(regex("^[A-Za-z0-9-]{8,40}$", var.join_code))
    error_message = "A join code has letters, digits and - only."
  }
}

variable "image_signing_key" {
  type        = string
  description = "The public key your images are signed with (forecrew image keygen prints it). Not a secret."
  validation {
    condition     = can(regex("^ed25519:[A-Za-z0-9+/=]{20,100}$", var.image_signing_key))
    error_message = "image_signing_key is an ed25519: public key line."
  }
}

variable "agent_version" {
  type        = string
  default     = "latest"
  description = "Release of the fleet agent to install."
  validation {
    condition     = can(regex("^(latest|v[0-9][A-Za-z0-9.+-]*)$", var.agent_version))
    error_message = "agent_version is latest or a release tag such as v0.2.0."
  }
}

variable "install_ref" {
  type        = string
  description = "Release tag of forecrew/forecrew the install script comes from, such as v0.2.0. A tag, never a branch: the instance runs that script as root. Read it first."
  validation {
    condition     = can(regex("^v[0-9][A-Za-z0-9._-]{0,62}$", var.install_ref))
    error_message = "install_ref is a release tag such as v0.2.0."
  }
}

variable "vpc_id" {
  type        = string
  description = "The VPC machines start in (ec2 mode)."
  validation {
    condition     = can(regex("^vpc-[0-9a-f]{8,17}$", var.vpc_id))
    error_message = "vpc_id is a VPC ID."
  }
}

variable "agent_subnet_id" {
  type        = string
  description = "Where the agent's own instance runs. It needs outbound HTTPS."
  validation {
    condition     = can(regex("^subnet-[0-9a-f]{8,17}$", var.agent_subnet_id))
    error_message = "agent_subnet_id is a subnet ID."
  }
}

variable "machine_subnet_ids" {
  type        = list(string)
  description = "One to three subnets for machines (ec2 mode), usually one per availability zone. Machines need outbound internet, directly or through a NAT gateway."
  validation {
    condition     = length(var.machine_subnet_ids) >= 1 && length(var.machine_subnet_ids) <= 3 && alltrue([for s in var.machine_subnet_ids : can(regex("^subnet-[0-9a-f]{8,17}$", s))])
    error_message = "machine_subnet_ids lists one to three subnet IDs."
  }
}

variable "host_availability_zone" {
  type        = string
  default     = ""
  description = "mac mode only: the availability zone of agent_subnet_id, for the dedicated host."
  validation {
    condition     = can(regex("^([a-z]{2}-[a-z]+-[0-9][a-z])?$", var.host_availability_zone))
    error_message = "host_availability_zone is an availability zone such as eu-west-1a."
  }
}

variable "fleet_name" {
  type        = string
  default     = ""
  description = "The value of the forecrew:fleet tag on everything the agent creates. The role allows exactly this, and the agent terminates every instance in the account and region that carries it, so it must be unique there. Empty means forecrew- and a hash of the account, region and VPC."
  validation {
    condition     = can(regex("^([A-Za-z0-9._-]{1,64})?$", var.fleet_name))
    error_message = "fleet_name has 1 to 64 letters, digits and . _ -."
  }
}

variable "instance_types" {
  type        = list(string)
  default     = ["c7i.large", "c7i.xlarge", "c7i.2xlarge", "c7i.4xlarge"]
  description = "The instance types machines may use (ec2 mode); * works, as in c7i.*. The role allows these and no others."
  validation {
    condition     = length(var.instance_types) >= 1 && alltrue([for t in var.instance_types : can(regex("^[a-z0-9][a-z0-9.*-]{0,40}$", t))])
    error_message = "instance_types lists instance types or patterns such as c7i.*."
  }
}

variable "assign_public_ip" {
  type        = bool
  default     = false
  description = "true for a public subnet. A private subnet with a NAT gateway needs none."
}

variable "max_machines" {
  type        = number
  default     = 10
  description = "The most machines at once, whatever Forecrew asks for."
  validation {
    condition     = var.max_machines >= 1 && var.max_machines <= 1000
    error_message = "max_machines is between 1 and 1000."
  }
}

variable "monthly_budget" {
  type        = number
  default     = 0
  description = "Stops new machines when the month's estimated spend reaches it, in the currency of your shapes' prices. 0 is no limit. The location shows a warning at 80%."
  validation {
    condition     = var.monthly_budget >= 0
    error_message = "monthly_budget must not be negative."
  }
}

variable "max_lifetime_hours" {
  type        = number
  default     = 24
  description = "No machine lives longer: it shuts itself down, and the agent terminates what outlives it."
  validation {
    condition     = var.max_lifetime_hours >= 1 && var.max_lifetime_hours <= 336
    error_message = "max_lifetime_hours is between 1 and 336."
  }
}

variable "agent_instance_type" {
  type        = string
  default     = "t3.small"
  description = "ec2: t3.small is enough. microvm: a metal type, or one with nested virtualization, sized for the microVMs. mac: mac2.metal or another Mac type."
  validation {
    condition     = can(regex("^[a-z0-9]+\\.[a-z0-9]+$", var.agent_instance_type))
    error_message = "agent_instance_type is an instance type such as t3.small."
  }
}

variable "agent_ami_id" {
  type        = string
  default     = ""
  validation {
    condition     = can(regex("^(ami-[0-9a-f]{8,17})?$", var.agent_ami_id))
    error_message = "agent_ami_id is an AMI ID, or empty."
  }
  description = "The agent's AMI; empty means the latest Amazon Linux 2023 for x86_64. For an Arm instance type, or mac mode, give one."
}

variable "copy_ami" {
  type = object({
    id     = string
    region = string
    digest = string
  })
  default     = null
  validation {
    condition = var.copy_ami == null ? true : (can(regex("^ami-[0-9a-f]{8,17}$", var.copy_ami.id)) &&
    can(regex("^[a-z]{2}-[a-z]+-[0-9]$", var.copy_ami.region)) && can(regex("^sha256:[0-9a-f]{64}$", var.copy_ami.digest)))
    error_message = "copy_ami needs an AMI ID, its region and the sha256: digest of the manifest."
  }
  description = "Optional. An AMI of your Forecrew image to copy into this account, tagged with the digest its signature covers (the digest in the manifest). The agent boots only AMIs of this account."
}
