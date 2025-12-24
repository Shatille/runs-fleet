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

source "amazon-ebs" "runner_base_arm64" {
  ami_name      = "runner-base-arm64-{{timestamp}}"
  instance_type = "c7g.xlarge"
  region        = var.region

  source_ami_filter {
    filters = {
      name                = "al2023-ami-*-arm64"
      virtualization-type = "hvm"
    }
    most_recent = true
    owners      = ["amazon"]
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
