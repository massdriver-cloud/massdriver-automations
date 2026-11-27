locals {
  # Attribute values with commas become lists, which match any of their values.
  config = merge(var.config, {
    match   = { for k, v in coalesce(var.config.match, {}) : k => [for x in split(",", v) : trimspace(x)] }
    exclude = { for k, v in coalesce(var.config.exclude, {}) : k => [for x in split(",", v) : trimspace(x)] }
    mentions = [
      for m in coalesce(var.config.mentions, []) : {
        match    = { for k, v in coalesce(m.match, {}) : k => [for x in split(",", v) : trimspace(x)] }
        mentions = m.mentions
      }
    ]
  })

  automation = {
    name  = "stale-proposals"
    image = local.image
    files = { (local.config_file) = yamlencode(local.config) }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, local.slack_secrets, {
      STALE_PROPOSALS_RUN_TOKEN = lookup(local.secrets, "STALE_PROPOSALS_RUN_TOKEN", "")
    })
    # The automation reads proposals from the API. The destination exists for its signing secret, and sends nothing.
    event_types = ["deployment.proposed"]
    attributes  = {}
    condition   = "false"
  }
}
