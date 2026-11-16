# One EC2 instance of the release, launched from the release's registered image (cloud_image), or image_id with the
# cloud-config as its user data.

locals {
  sgs = compact(split(",", var.security_group_ids))
}

data "aws_subnet" "vm" {
  id = var.subnet_id
}

# Without security_group_ids, a group of what the VM's own firewall opens (stowage_vm_seed's
# firewall): SSH from its sources, each port from its own; every source out.
resource "aws_security_group" "vm" {
  count       = length(local.sgs) > 0 ? 0 : 1
  name        = "${var.name}-stowage"
  description = "SSH and the ports the release opens"
  vpc_id      = data.aws_subnet.vm.vpc_id

  dynamic "ingress" {
    for_each = concat([for s in local.seed.firewall.ssh_sources : { protocol = "tcp", port = 22, source = s }],
    flatten([for p in local.seed.firewall.open_ports : [for s in p.sources : { protocol = p.protocol, port = p.port, source = s }]]))
    content {
      protocol         = ingress.value.protocol
      from_port        = ingress.value.port
      to_port          = ingress.value.port
      cidr_blocks      = strcontains(ingress.value.source, ":") ? null : [ingress.value.source]
      ipv6_cidr_blocks = strcontains(ingress.value.source, ":") ? [ingress.value.source] : null
    }
  }

  egress {
    protocol         = "-1"
    from_port        = 0
    to_port          = 0
    cidr_blocks      = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }
}

resource "aws_instance" "vm" {
  ami                    = local.image_id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = length(local.sgs) > 0 ? local.sgs : [aws_security_group.vm[0].id]
  user_data_base64       = base64encode(local.seed.user_data)

  root_block_device {
    volume_size = local.disk_gib
    volume_type = "gp3"
  }

  metadata_options {
    http_tokens = "required"
  }

  tags = {
    Name = var.name
  }

  lifecycle {
    # A later release reaches the instance as an upgrade, never as a replacement.
    ignore_changes = [ami, user_data_base64]
  }
}

# The Elastic IP eip_allocation_id names, reserved beforehand: the instance's public address,
# released from it (never deleted) by a destroy.
resource "aws_eip_association" "vm" {
  count         = var.eip_allocation_id == "" ? 0 : 1
  instance_id   = aws_instance.vm.id
  allocation_id = var.eip_allocation_id
}

data "aws_eip" "given" {
  count = var.eip_allocation_id == "" ? 0 : 1
  id    = var.eip_allocation_id
}

# The image `ship --disks` registered for this release's disk (cloud_image), else image_id.
data "aws_ami" "release" {
  count       = var.image_id == "" ? 1 : 0
  owners      = ["self"]
  most_recent = true
  filter {
    name   = "name"
    values = [local.seed.disk["aws"].cloud_image]
  }
}

locals {
  image_id = var.image_id != "" ? var.image_id : data.aws_ami.release[0].id
}

locals {
  # The address SSH reaches the VM at: vm_address, else its public one.
  address = coalesce(var.vm_address, var.eip_allocation_id == "" ? coalesce(aws_instance.vm.public_ip, aws_instance.vm.private_ip) : data.aws_eip.given[0].public_ip)
}
