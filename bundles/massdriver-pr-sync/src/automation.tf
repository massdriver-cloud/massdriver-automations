locals {
  automation = {
    name  = "pr-sync"
    image = local.image
    files = { (local.config_file) = yamlencode(var.config) }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, {
      GITHUB_TOKEN = lookup(local.secrets, "GITHUB_TOKEN", "")
    })
    # The automation reads GitHub on an interval. The destination exists for its signing secret, and sends nothing.
    event_types = ["deployment.completed"]
    attributes  = {}
    condition   = "false"
  }
}
