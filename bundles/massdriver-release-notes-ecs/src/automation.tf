locals {
  automation = {
    name  = "release-notes"
    image = local.image
    files = { (local.config_file) = yamlencode(var.config) }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, local.slack_secrets, {
      TEAMS_WEBHOOK_URL = lookup(local.secrets, "TEAMS_WEBHOOK_URL", "")
    })
    event_types = ["repo.published"]
    attributes  = { "md-repo-artifact-type" = "bundle" }
  }
}
