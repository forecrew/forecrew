# Forecrew in your own AWS account (docs/runbooks/byoc-aws.md). The agent's
# instance role can only create, tag and terminate instances that carry the fleet
# tag; policy.json.tftpl is the whole of it.

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

data "aws_ssm_parameter" "al2023" {
  count = var.agent_ami_id == "" ? 1 : 0
  name  = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

locals {
  fleet_name = var.fleet_name

  is_ec2   = var.mode == "ec2"
  is_mac   = var.mode == "mac"
  has_copy = local.is_ec2 && var.copy_ami != null

  agent_kind = local.is_ec2 ? "aws" : (local.is_mac ? "tart" : "microvm")
  agent_ami  = var.agent_ami_id != "" ? var.agent_ami_id : data.aws_ssm_parameter.al2023[0].value

  subnet_arns = [for s in var.machine_subnet_ids : "arn:aws:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:subnet/${s}"]
  tags        = { "forecrew:kit" = "aws-byoc" }

  # What machines can reach: the whole IPv4 space but 0/8, 10/8, 100.64/10, 127/8,
  # 169.254/16, 172.16/12, 192.168/16 and 224/3. A security group cannot say
  # "everything but", so these are the public ranges, spelled out.
  public_cidrs = [
    "1.0.0.0/8", "2.0.0.0/7", "4.0.0.0/6", "8.0.0.0/7", "11.0.0.0/8", "12.0.0.0/6", "16.0.0.0/4", "32.0.0.0/3",
    "64.0.0.0/3", "96.0.0.0/6", "100.0.0.0/10", "100.128.0.0/9", "101.0.0.0/8", "102.0.0.0/7", "104.0.0.0/5",
    "112.0.0.0/5", "120.0.0.0/6", "124.0.0.0/7", "126.0.0.0/8", "128.0.0.0/3", "160.0.0.0/5", "168.0.0.0/8",
    "169.0.0.0/9", "169.128.0.0/10", "169.192.0.0/11", "169.224.0.0/12", "169.240.0.0/13", "169.248.0.0/14",
    "169.252.0.0/15", "169.255.0.0/16", "170.0.0.0/7", "172.0.0.0/12", "172.32.0.0/11", "172.64.0.0/10",
    "172.128.0.0/9", "173.0.0.0/8", "174.0.0.0/7", "176.0.0.0/4", "192.0.0.0/9", "192.128.0.0/11", "192.160.0.0/13",
    "192.169.0.0/16", "192.170.0.0/15", "192.172.0.0/14", "192.176.0.0/12", "192.192.0.0/10", "193.0.0.0/8",
    "194.0.0.0/7", "196.0.0.0/6", "200.0.0.0/5", "208.0.0.0/4",
  ]
}

resource "aws_security_group" "machines" {
  count       = local.is_ec2 ? 1 : 0
  name_prefix = "${local.fleet_name}-machines-"
  description = "Forecrew machines - no inbound, the public internet out"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { Name = "${local.fleet_name}-machines" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_egress_rule" "machines_public" {
  for_each          = local.is_ec2 ? toset(local.public_cidrs) : toset([])
  security_group_id = aws_security_group.machines[0].id
  ip_protocol       = "-1"
  cidr_ipv4         = each.value
}

resource "aws_vpc_security_group_egress_rule" "machines_dns_udp" {
  count             = local.is_ec2 ? 1 : 0
  security_group_id = aws_security_group.machines[0].id
  ip_protocol       = "udp"
  from_port         = 53
  to_port           = 53
  cidr_ipv4         = "169.254.169.253/32"
  description       = "VPC DNS resolver"
}

resource "aws_vpc_security_group_egress_rule" "machines_dns_tcp" {
  count             = local.is_ec2 ? 1 : 0
  security_group_id = aws_security_group.machines[0].id
  ip_protocol       = "tcp"
  from_port         = 53
  to_port           = 53
  cidr_ipv4         = "169.254.169.253/32"
  description       = "VPC DNS resolver"
}

resource "aws_vpc_security_group_egress_rule" "machines_ntp" {
  count             = local.is_ec2 ? 1 : 0
  security_group_id = aws_security_group.machines[0].id
  ip_protocol       = "udp"
  from_port         = 123
  to_port           = 123
  cidr_ipv4         = "169.254.169.123/32"
  description       = "Amazon Time Sync"
}

resource "aws_security_group" "agent" {
  name_prefix = "${local.fleet_name}-agent-"
  description = "Forecrew fleet agent - no inbound, HTTPS and DNS out"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { Name = "${local.fleet_name}-agent" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_egress_rule" "agent_https" {
  for_each          = toset(local.public_cidrs)
  security_group_id = aws_security_group.agent.id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = each.value
}

resource "aws_vpc_security_group_egress_rule" "agent_dns_udp" {
  security_group_id = aws_security_group.agent.id
  ip_protocol       = "udp"
  from_port         = 53
  to_port           = 53
  cidr_ipv4         = "169.254.169.253/32"
  description       = "VPC DNS resolver"
}

resource "aws_vpc_security_group_egress_rule" "agent_dns_tcp" {
  security_group_id = aws_security_group.agent.id
  ip_protocol       = "tcp"
  from_port         = 53
  to_port           = 53
  cidr_ipv4         = "169.254.169.253/32"
  description       = "VPC DNS resolver"
}

resource "aws_vpc_security_group_egress_rule" "agent_ntp" {
  security_group_id = aws_security_group.agent.id
  ip_protocol       = "udp"
  from_port         = 123
  to_port           = 123
  cidr_ipv4         = "169.254.169.123/32"
  description       = "Amazon Time Sync"
}

resource "aws_iam_role" "agent" {
  count = local.is_ec2 ? 1 : 0
  name_prefix = "${local.fleet_name}-agent-"
  description = "Forecrew fleet agent. It creates, tags and terminates instances that carry its fleet tag, and nothing else."
  tags        = local.tags
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "agent" {
  count = local.is_ec2 ? 1 : 0
  name  = "forecrew-fleet-agent"
  role  = aws_iam_role.agent[0].id
  policy = templatefile("${path.module}/policy.json.tftpl", {
    region              = data.aws_region.current.name
    account             = data.aws_caller_identity.current.account_id
    fleet               = local.fleet_name
    instance_types      = jsonencode(var.instance_types)
    subnet_arns         = jsonencode(local.subnet_arns)
    network_arns        = jsonencode(concat(local.subnet_arns, ["arn:aws:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:security-group/${aws_security_group.machines[0].id}"]))
  })
}

resource "aws_iam_instance_profile" "agent" {
  count       = local.is_ec2 ? 1 : 0
  name_prefix = "${local.fleet_name}-agent-"
  role        = aws_iam_role.agent[0].name
  tags        = local.tags
}

resource "aws_ec2_host" "mac" {
  count             = local.is_mac ? 1 : 0
  availability_zone = var.host_availability_zone
  instance_type     = var.agent_instance_type
  auto_placement    = "off"
  host_recovery     = "on"
  tags              = merge(local.tags, { Name = "${local.fleet_name}-mac-host" })
}

resource "aws_instance" "agent" {
  ami                    = local.agent_ami
  instance_type          = var.agent_instance_type
  subnet_id              = var.agent_subnet_id
  vpc_security_group_ids = [aws_security_group.agent.id]
  iam_instance_profile   = local.is_ec2 ? aws_iam_instance_profile.agent[0].name : null
  tenancy                = local.is_mac ? "host" : null
  host_id                = local.is_mac ? aws_ec2_host.mac[0].id : null
  tags                   = merge(local.tags, { Name = "${local.fleet_name}-agent" })

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_size           = 40
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
  }

  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    image_signing_key      = var.image_signing_key
    agent_kind             = local.agent_kind
    region                 = data.aws_region.current.name
    fleet_name             = local.fleet_name
    machine_subnets        = join(", ", var.machine_subnet_ids)
    machine_security_group = local.is_ec2 ? aws_security_group.machines[0].id : "none"
    instance_types         = join(", ", var.instance_types)
    assign_public_ip       = var.assign_public_ip
    max_machines           = var.max_machines
    monthly_budget         = var.monthly_budget
    max_lifetime_hours     = var.max_lifetime_hours
    join_code              = var.join_code
    install_ref            = var.install_ref
    server_url             = var.server_url
    agent_version          = var.agent_version
  })
  user_data_replace_on_change = true
}

# Optional: a copy of the image's AMI in this account, tagged with the digest its
# signature covers. The agent boots only AMIs of this account.
resource "aws_ami_copy" "image" {
  count             = local.has_copy ? 1 : 0
  name              = "forecrew-${substr(var.copy_ami.digest, 7, 12)}"
  source_ami_id     = var.copy_ami.id
  source_ami_region = var.copy_ami.region
  tags              = merge(local.tags, { "forecrew:image-digest" = var.copy_ami.digest })
}
