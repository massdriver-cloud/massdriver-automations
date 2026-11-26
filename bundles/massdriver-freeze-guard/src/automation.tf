locals {
  # Attribute values with commas become lists, which match any of their values.
  config = merge(var.config, {
    windows = [
      for w in coalesce(var.config.windows, []) : merge(w, {
        match = { for k, v in coalesce(w.match, {}) : k => [for x in split(",", v) : trimspace(x)] }
      })
    ]
  })

  automation = {
    name  = "freeze-guard"
    image = local.image
    files = { (local.config_file) = yamlencode(local.config) }
    state = true
    env   = merge(local.massdriver_env, { STATE_DIR = local.state_dir })
    secrets = merge(local.massdriver_secrets, local.slack_secrets, {
      FREEZE_ADMIN_TOKEN = lookup(local.secrets, "FREEZE_ADMIN_TOKEN", "")
    })
    # Group events have no attributes, and freeze-guard reconciles every group on each one.
    event_types = ["group.created", "group.updated", "group.deleted"]
  }
}
