locals {
  # Attribute values with commas become lists, which match any of their values.
  config = merge(var.config, {
    trains = [
      for t in coalesce(var.config.trains, []) : merge(t, {
        stages = [
          for s in t.stages : merge(s, {
            match = { for k, v in s.match : k => [for x in split(",", v) : trimspace(x)] }
          })
        ]
      })
    ]
  })

  automation = {
    name  = "upgrade-train"
    image = local.image
    files = { (local.config_file) = yamlencode(local.config) }
    state = true
    env   = merge(local.massdriver_env, { STATE_DIR = local.state_dir })
    secrets = merge(local.massdriver_secrets, local.slack_secrets, {
      UPGRADE_TRAIN_TOKEN = lookup(local.secrets, "UPGRADE_TRAIN_TOKEN", "")
    })
    event_types = ["repo.published", "deployment.completed", "deployment.failed", "deployment.aborted", "deployment.rejected"]
    attributes  = {}
    condition   = ".type == \"repo.published\" or ((.data.entity.message // \"\") | startswith(\"upgrade-train: \"))"
  }
}
