locals {
  # Attribute values with commas become lists, which match any of their values.
  config = merge(var.config, {
    mappings = [
      for m in coalesce(var.config.mappings, []) : merge(m, {
        match = { for k, v in m.match : k => [for x in split(",", v) : trimspace(x)] }
      })
    ]
  })

  automation = {
    name  = "params-to-git"
    image = local.image
    files = { (local.config_file) = yamlencode(local.config) }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, {
      GITHUB_TOKEN    = lookup(local.secrets, "GITHUB_TOKEN", "")
      PARAMS_MASK_KEY = lookup(local.secrets, "PARAMS_MASK_KEY", "")
    })
    event_types = ["deployment.completed"]
    attributes  = coalesce(var.attributes, {})
    # The mappings are left to the automation, which checks them on each event.
    condition = ".data.metadata.deployment_action != \"PLAN\""
  }
}
