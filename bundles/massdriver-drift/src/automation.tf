locals {
  # Attribute values with commas become lists, which match any of their values.
  config = merge(var.config, {
    checks = [
      for c in coalesce(var.config.checks, []) : merge(c, {
        match = { for k, v in c.match : k => [for x in split(",", v) : trimspace(x)] }
      })
    ]
  })

  automation = {
    name  = "drift"
    image = local.image
    files = { (local.config_file) = yamlencode(local.config) }
    state = true
    env   = merge(local.massdriver_env, { STATE_DIR = local.state_dir })
    secrets = merge(local.massdriver_secrets, local.slack_secrets, {
      DRIFT_RUN_TOKEN = lookup(local.secrets, "DRIFT_RUN_TOKEN", "")
    })
    event_types = ["deployment.completed", "deployment.failed", "deployment.aborted"]
    attributes  = coalesce(var.attributes, {})
    condition   = ".data.metadata.deployment_action == \"PLAN\""
  }
}
