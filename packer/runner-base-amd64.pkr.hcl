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
    filters = {
      name                = "al2023-ami-*-x86_64"
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
    Name           = "runner-base-amd64"
    Version        = var.ami_version
    OS             = "Amazon Linux 2023"
    Architecture   = "amd64"
    Docker         = "installed"
    ManagedBy      = "Packer"
    BuildTimestamp = "{{timestamp}}"
    Stage          = "base"
  }

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

  provisioner "shell" {
    script = "${path.root}/provision-base.sh"
  }
}
