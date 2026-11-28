locals {
  automation = {
    name  = "oncall"
    image = local.image
    # Routes choose between PagerDuty, incident.io, and a fixed list, so the form holds the YAML of the config file.
    files = { (local.config_file) = var.config }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, {
      PAGERDUTY_TOKEN     = lookup(local.secrets, "PAGERDUTY_TOKEN", "")
      INCIDENT_IO_API_KEY = lookup(local.secrets, "INCIDENT_IO_API_KEY", "")
    })
    # The automation reads its providers on an interval. The destination exists for its signing secret, and sends
    # nothing.
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
