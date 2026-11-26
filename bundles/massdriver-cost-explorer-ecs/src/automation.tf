locals {
  automation = {
    name  = "cost-explorer"
    image = local.image
    files = { (local.config_file) = yamlencode(var.config) }
    state = true
    env   = merge(local.massdriver_env, { STATE_DIR = local.state_dir })
    secrets = merge(local.massdriver_secrets, {
      COST_EXPLORER_PASSWORD = lookup(local.secrets, "COST_EXPLORER_PASSWORD", "")
    })
    event_types = ["deployment.completed"]
    condition   = ".data.metadata.deployment_action == \"PROVISION\" or .data.metadata.deployment_action == \"DECOMMISSION\""
  }
}

resource "terraform_data" "checks" {
  lifecycle {
    precondition {
      condition     = try(var.config.public, false) == true || lookup(local.secrets, "COST_EXPLORER_PASSWORD", "") != ""
      error_message = "Set the COST_EXPLORER_PASSWORD secret, or turn on Public."
    }
  }
}

resource "massdriver_resource" "website" {
  field = "website"
  name  = "${local.name_prefix} cost dashboard"
  resource = jsonencode({
    url = "https://${local.host}/"
  })
}
