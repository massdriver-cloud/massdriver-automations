locals {
  automation = {
    name  = "leaderboard"
    image = local.image
    # Routes choose between GitHub and a fixed list, so the form holds the YAML of the config file.
    files = { (local.config_file) = var.config }
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

resource "terraform_data" "checks" {
  lifecycle {
    precondition {
      condition     = can(yamldecode(var.config))
      error_message = "The settings are not valid YAML."
    }
  }
}
