locals {
  automation = {
    name  = "proposal-chatops"
    image = local.image
    files = { (local.config_file) = yamlencode(var.config) }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, {
      SLACK_BOT_TOKEN      = lookup(local.secrets, "SLACK_BOT_TOKEN", "")
      SLACK_SIGNING_SECRET = lookup(local.secrets, "SLACK_SIGNING_SECRET", "")
    })
    event_types = ["deployment.proposed", "deployment.approved", "deployment.rejected"]
    attributes  = coalesce(var.attributes, {})
  }
}

resource "terraform_data" "checks" {
  lifecycle {
    precondition {
      condition     = alltrue([for k in ["SLACK_BOT_TOKEN", "SLACK_SIGNING_SECRET"] : lookup(local.secrets, k, "") != ""])
      error_message = "Set the SLACK_BOT_TOKEN and SLACK_SIGNING_SECRET secrets."
    }
  }
}
