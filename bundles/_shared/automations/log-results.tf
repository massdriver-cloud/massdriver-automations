locals {
  # Rules are free-form, so the form holds the YAML of the config file.
  config = try(yamldecode(var.config), {})
  # The same default as the automation, so the destination sends what it reads.
  actions = [for a in try(coalesce(local.config.actions), ["PROVISION", "PLAN"]) : upper(a)]

  automation = {
    name        = "log-results"
    image       = local.image
    files       = { (local.config_file) = var.config }
    env         = local.massdriver_env
    secrets     = local.massdriver_secrets
    event_types = ["deployment.completed", "deployment.failed"]
    attributes  = coalesce(var.attributes, {})
    condition   = join(" or ", [for a in local.actions : ".data.metadata.deployment_action == ${jsonencode(a)}"])
  }
}

resource "terraform_data" "checks" {
  lifecycle {
    precondition {
      condition     = can(yamldecode(var.config))
      error_message = "The settings are not valid YAML."
    }
    precondition {
      condition     = length(try(coalesce(local.config.rules), [])) > 0
      error_message = "Add at least one rule."
    }
  }
}
