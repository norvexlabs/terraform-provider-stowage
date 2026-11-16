# A Docker host on EC2 — Debian with Docker, made by cloud-init — and a Stowage product installed on
# it over SSH by stowage_docker_install. The host's SSH key is made here (tls_private_key) and given
# to the instance by cloud-init, so the install pins it from the first connection: no key is
# learnt on first contact. The installer runs on the host (`docker run`), never here.

# The host's own SSH key: its private half is in state (encrypt state) and in the user data.
resource "tls_private_key" "host" {
  algorithm = "ED25519"
}

data "aws_ami" "debian" {
  most_recent = true
  owners      = ["136693071363"] # Debian's
  filter {
    name   = "name"
    values = ["debian-13-amd64-*"]
  }
}

data "aws_subnet" "this" {
  id = var.subnet_id
}

resource "aws_security_group" "docker" {
  name        = "${var.name}-docker"
  description = "SSH from ssh_sources; HTTP and HTTPS from every source"
  vpc_id      = data.aws_subnet.this.vpc_id

  ingress {
    protocol    = "tcp"
    from_port   = 22
    to_port     = 22
    cidr_blocks = var.ssh_sources
  }
  dynamic "ingress" {
    for_each = [80, 443]
    content {
      protocol    = "tcp"
      from_port   = ingress.value
      to_port     = ingress.value
      cidr_blocks = ["0.0.0.0/0"]
    }
  }
  egress {
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
}

locals {
  user_data = <<-EOT
  #cloud-config
  ssh_deletekeys: true
  ssh_keys:
    ed25519_private: |
      ${indent(4, trimspace(tls_private_key.host.private_key_openssh))}
    ed25519_public: ${trimspace(tls_private_key.host.public_key_openssh)}
  ssh_authorized_keys:
    - ${trimspace(file(pathexpand("${var.ssh_key_file}.pub")))}
  package_update: true
  packages:
    - docker.io
  runcmd:
    - [systemctl, enable, --now, docker]
    - [usermod, -aG, docker, admin]
  EOT
}

resource "aws_instance" "docker" {
  ami                    = data.aws_ami.debian.id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = [aws_security_group.docker.id]
  user_data              = local.user_data

  root_block_device {
    volume_size = var.disk_gib
    volume_type = "gp3"
  }

  metadata_options {
    http_tokens = "required"
  }

  tags = {
    Name = var.name
  }

  lifecycle {
    ignore_changes = [ami, user_data]
  }
}

# Docker installed: cloud-init done, checked over SSH pinned to the host's key.
resource "terraform_data" "ready" {
  triggers_replace = [aws_instance.docker.id]

  connection {
    type        = "ssh"
    host        = aws_instance.docker.public_ip
    user        = "admin"
    private_key = file(pathexpand(var.ssh_key_file))
    host_key    = trimspace(tls_private_key.host.public_key_openssh)
  }

  provisioner "remote-exec" {
    inline = ["cloud-init status --wait >/dev/null; docker version >/dev/null 2>&1 || sudo -n docker version >/dev/null"]
  }
}

resource "stowage_docker_install" "app" {
  installer        = var.installer
  host             = aws_instance.docker.public_ip
  user             = "admin"
  private_key_file = pathexpand(var.ssh_key_file)
  known_hosts_line = "${aws_instance.docker.public_ip} ${trimspace(tls_private_key.host.public_key_openssh)}"
  work_dir         = "/home/admin"

  inputs                   = var.inputs
  secret_inputs_wo         = var.secret_inputs
  secret_inputs_wo_version = 1
  tls                      = var.tls
  domains                  = var.domains

  deletion_protection = var.deletion_protection

  depends_on = [terraform_data.ready]
}

output "address" {
  value = aws_instance.docker.public_ip
}

output "version" {
  value = stowage_docker_install.app.version
}
