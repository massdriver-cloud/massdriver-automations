locals {
  # Rules and scenarios are free-form, so the form holds the YAML of the config file.
  config   = try(yamldecode(var.config), {})
  mode     = try(coalesce(local.config.mode), "sync")
  aws_keys = lookup(local.secrets, "AWS_ACCESS_KEY_ID", "") != ""
  uses_cloudwatch = local.mode == "sync" && (
    anytrue([for r in try(coalesce(local.config.rules), []) : try(r.cloudwatch, null) != null]) ||
    anytrue([for p in try(coalesce(local.config.presets), []) : startswith(try(p.preset, ""), "aws-")])
  )

  automation = {
    name  = "stash-sync"
    image = local.image
    files = { (local.config_file) = var.config }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, {
      AWS_ACCESS_KEY_ID     = lookup(local.secrets, "AWS_ACCESS_KEY_ID", "")
      AWS_SECRET_ACCESS_KEY = lookup(local.secrets, "AWS_SECRET_ACCESS_KEY", "")
      PROMETHEUS_TOKEN      = lookup(local.secrets, "PROMETHEUS_TOKEN", "")
      PROMETHEUS_PASSWORD   = lookup(local.secrets, "PROMETHEUS_PASSWORD", "")
    })
    # The automation reads its sources on an interval. The destination exists for its signing secret, and sends
    # nothing.
    event_types     = ["deployment.completed"]
    attributes      = {}
    condition       = "false"
    cloudwatch_read = local.uses_cloudwatch && !local.aws_keys
  }
}

resource "terraform_data" "checks" {
  lifecycle {
    precondition {
      condition     = can(yamldecode(var.config))
      error_message = "The settings are not valid YAML."
    }
  }
}
