locals {
  # URLs that people or other services use, which are easy to lose track of.
  endpoints = {
    statuspage = [
      { name = "Status page", url = "https://${local.statuspage_host}/" },
      { name = "Status JSON", url = "https://${local.statuspage_host}/api/status.json" },
    ]
    cost-explorer = [
      { name = "Dashboard", url = "https://${local.cost_explorer_host}/" },
      { name = "Costs JSON", url = "https://${local.cost_explorer_host}/api/costs.json" },
    ]
    proposal-chatops = [
      { name = "Slack interactivity URL", url = "https://${local.automations["proposal-chatops"].host}/slack/interactions" },
    ]
    drift = lookup(local.secrets, "DRIFT_RUN_TOKEN", "") == "" ? [] : [
      { name = "Start a check", url = "https://${local.automations.drift.host}/drift/run?check=<name>" },
    ]
    freeze-guard = lookup(local.secrets, "FREEZE_GUARD_ADMIN_TOKEN", "") == "" ? [] : [
      { name = "Lift a window", url = "https://${local.automations["freeze-guard"].host}/freeze/lift?window=<name>" },
    ]
  }
}

resource "massdriver_resource" "automations" {
  count = length(local.enabled) > 0 ? 1 : 0

  field = "automations"
  name  = "${local.name_prefix} automations"
  resource = jsonencode({
    automations = [
      for name, a in local.enabled : {
        name = name
        url  = "https://${a.host}"
        destination = merge(
          {
            id          = massdriver_event_destination.main[name].id
            name        = massdriver_event_destination.main[name].name
            event_types = a.event_types
            attributes  = a.attributes
          },
          trimspace(a.condition) == "" ? {} : { condition = trimspace(a.condition) },
        )
        endpoints = lookup(local.endpoints, name, [])
      }
    ]
  })
}
