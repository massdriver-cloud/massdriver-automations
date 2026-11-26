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

  # Massdriver writes one identity token for each level; the role's trust policy accepts the level it names.
  assume_role_with_web_identity {
    role_arn                = var.aws_authentication.arn
    session_name            = var.md_metadata.name_prefix
    web_identity_token_file = "/massdriver/oidc/${var.aws_authentication.token_level}.jwt"
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
