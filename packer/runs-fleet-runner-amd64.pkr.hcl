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

source "amazon-ebs" "runs_fleet_runner_amd64" {
  ami_name             = "runs-fleet-runner-amd64-{{timestamp}}"
  instance_type        = "c7i.xlarge"
  region               = var.region
  iam_instance_profile = "runs-fleet-runner"

  source_ami_filter {
    filters = {
      name                = "runner-base-amd64-*"
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
    Name           = "runs-fleet-runner-amd64"
    Version        = var.ami_version
    OS             = "Amazon Linux 2023"
    Architecture   = "amd64"
    Runner         = "latest"
    ManagedBy      = "Packer"
    BuildTimestamp = "{{timestamp}}"
    Stage          = "application"
    BaseAMI        = "runner-base-amd64"
  }

  run_tags = {
    Name = "packer-runs-fleet-runner-amd64-builder"
  }
}

build {
  sources = ["source.amazon-ebs.runs_fleet_runner_amd64"]

  provisioner "shell" {
    environment_vars = [
      "RUNNER_ARCH=x64"
    ]
    script = "${path.root}/provision-runs-fleet.sh"
  }
}
