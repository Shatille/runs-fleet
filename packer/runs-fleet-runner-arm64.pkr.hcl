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

variable "security_group_id" {
  type        = string
  description = "Security group ID for packer builder instance"
}

variable "subnet_id" {
  type        = string
  description = "Subnet ID for packer builder instance"
}

source "amazon-ebs" "runs_fleet_runner_arm64" {
  ami_name             = "runs-fleet-runner-arm64-{{timestamp}}"
  instance_type        = "c7g.xlarge"
  region               = var.region
  iam_instance_profile = "runs-fleet-runner"
  security_group_id    = var.security_group_id
  subnet_id            = var.subnet_id

  source_ami_filter {
    filters = {
      name                = "runner-base-arm64-*"
      virtualization-type = "hvm"
    }
    most_recent = true
    owners      = ["self"]
  }

  ssh_username = "ec2-user"

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

  tags = {
    Name           = "runs-fleet-runner-arm64"
    Version        = var.ami_version
    OS             = "Amazon Linux 2023"
    Architecture   = "arm64"
    Runner         = "latest"
    ManagedBy      = "Packer"
    BuildTimestamp = "{{timestamp}}"
    Stage          = "application"
    BaseAMI        = "runner-base-arm64"
  }

  run_tags = {
    Name       = "packer-runs-fleet-runner-arm64-builder"
    created-by = "packer"
  }

  run_volume_tags = {
    created-by = "packer"
  }

  snapshot_tags = {
    created-by = "packer"
  }
}

build {
  sources = ["source.amazon-ebs.runs_fleet_runner_arm64"]

  provisioner "shell" {
    environment_vars = [
      "RUNNER_ARCH=arm64"
    ]
    script = "${path.root}/provision-runs-fleet.sh"
  }
}
