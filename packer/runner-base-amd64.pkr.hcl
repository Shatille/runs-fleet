packer {
  required_plugins {
    amazon = {
      version = ">= 1.0.0"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

variable "region" {
  type    = string
  default = "ap-northeast-1"
}

variable "ami_version" {
  type    = string
  default = "1.0.0"
}

variable "vpc_id" {
  type        = string
  description = "VPC ID for packer builder (subnet and SG are filter-discovered)"
}

variable "extra_tags" {
  type        = map(string)
  default     = {}
  description = "Additional tags merged onto the final AMI. Intended for downstream forks/environments. Keys take precedence over the built-in tag set (Stage, Architecture, etc.) — pick non-colliding names unless overriding is intentional."
}

source "amazon-ebs" "runner_base_amd64" {
  ami_name             = "runner-base-amd64-{{timestamp}}"
  instance_type        = "c6i.xlarge"
  region               = var.region
  iam_instance_profile = "runs-fleet-runner"
  vpc_id               = var.vpc_id

  subnet_filter {
    filters = {
      "vpc-id" = var.vpc_id
    }
    most_free = true
    random    = false
  }

  security_group_filter {
    filters = {
      "group-name" = "runs-fleet-runner"
    }
  }

  source_ami_filter {
    # Pinned to AL2023 standard + kernel-6.1 (the default LTS). The broader
    # `al2023-ami-*-x86_64` glob also matches `al2023-ami-minimal-*`, which
    # ships without amazon-ssm-agent — Packer's session_manager SSH then
    # times out.
    filters = {
      name                = "al2023-ami-2023.*-kernel-6.1-x86_64"
      virtualization-type = "hvm"
    }
    most_recent = true
    owners      = ["amazon"]
  }

  communicator  = "ssh"
  ssh_username  = "ec2-user"
  ssh_interface = "session_manager"

  launch_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = 30
    volume_type           = "gp3"
    delete_on_termination = true
  }

  ami_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = 30
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = merge({
    Name           = "runner-base-amd64"
    Version        = var.ami_version
    OS             = "Amazon Linux 2023"
    Architecture   = "amd64"
    Docker         = "installed"
    ManagedBy      = "Packer"
    BuildTimestamp = "{{timestamp}}"
    Stage          = "base"
  }, var.extra_tags)

  run_tags = {
    Name       = "packer-runner-base-amd64-builder"
    created-by = "runs-fleet-packer"
  }

  run_volume_tags = {
    created-by = "runs-fleet-packer"
  }

  snapshot_tags = {
    created-by = "runs-fleet-packer"
  }
}

build {
  sources = ["source.amazon-ebs.runner_base_amd64"]

  # Downstream extension hook. Upstream ships an empty stub at
  # packer/provision-base-hook.sh; the build-base-ami workflow rewrites it
  # from the PROVISION_BASE_HOOK secret when set, otherwise it stays empty.
  provisioner "file" {
    source      = "${path.root}/provision-base-hook.sh"
    destination = "/tmp/provision-base-hook.sh"
  }

  provisioner "shell" {
    script = "${path.root}/provision-base.sh"
  }

  # Vulnerability gate: scan the provisioned filesystem before the AMI snapshot
  # is taken. Reuses the same Trivy config + VEX as the container path. If the
  # scan finds an unsuppressed HIGH/CRITICAL finding, the build fails and no
  # AMI is registered.
  provisioner "file" {
    source      = "${path.root}/../.trivy/trivy.yaml"
    destination = "/tmp/trivy.yaml"
  }

  provisioner "file" {
    source      = "${path.root}/../.trivy/vex.json"
    destination = "/tmp/vex.json"
  }

  provisioner "file" {
    source      = "${path.root}/../.trivy/gate.sh"
    destination = "/tmp/gate.sh"
  }

  provisioner "shell" {
    script = "${path.root}/provision-trivy-scan.sh"
  }
}
