terraform {
  required_version = ">= 1.10"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    massdriver = {
      source = "massdriver-cloud/massdriver"
      # 2.7.0 adds signing_secret on massdriver_event_destination.
      version = "~> 2.7"
    }
  }
}

provider "aws" {
  region = var.ecs_cluster.region

  assume_role {
    role_arn    = var.aws_authentication.arn
    external_id = try(var.aws_authentication.external_id, null)
  }

  default_tags {
    tags = var.md_metadata.default_tags
  }
}

# A deployment's own token cannot create event destinations, so the provider uses the service account.
provider "massdriver" {
  url             = var.service_account.url
  organization_id = var.service_account.organization_id
  api_key         = var.service_account.api_key
}

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}
