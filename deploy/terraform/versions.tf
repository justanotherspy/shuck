# shuck v2 self-hosted backend — serverless deployment target (JUS-92).
# See README.md for the end-to-end setup walkthrough.

terraform {
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.6"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }
}

provider "aws" {
  # null falls back to AWS_REGION / the active profile.
  region = var.region

  default_tags {
    tags = merge({ "shuck:stack" = var.name_prefix }, var.tags)
  }
}
