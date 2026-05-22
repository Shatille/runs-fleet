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

source "amazon-ebs" "runner_base_arm64" {
  ami_name             = "runner-base-arm64-{{timestamp}}"
  instance_type        = "c7g.xlarge"
  region               = var.region
  iam_instance_profile = "runs-fleet-runner"
  security_group_id    = var.security_group_id
  subnet_id            = var.subnet_id

  source_ami_filter {
    filters = {
      name                = "al2023-ami-*-arm64"
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

  tags = {
    Name           = "runner-base-arm64"
    Version        = var.ami_version
    OS             = "Amazon Linux 2023"
    Architecture   = "arm64"
    Docker         = "installed"
    ManagedBy      = "Packer"
    BuildTimestamp = "{{timestamp}}"
    Stage          = "base"
  }

  run_tags = {
    Name = "packer-runner-base-arm64-builder"
  }
}

build {
  sources = ["source.amazon-ebs.runner_base_arm64"]

  provisioner "shell" {
    script = "${path.root}/provision-base.sh"
  }
}
