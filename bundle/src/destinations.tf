resource "massdriver_event_destination" "main" {
  for_each = local.enabled

  name    = "${local.name_prefix}-${each.key}"
  adapter = "HTTP"
  config = jsonencode({
    url = "https://${each.value.host}/hooks/massdriver"
  })

  event_types = each.value.event_types
  attributes  = each.value.attributes
  condition   = trimspace(each.value.condition) != "" ? trimspace(each.value.condition) : null

  lifecycle {
    precondition {
      condition     = each.key != "audit-archiver" || length(setintersection(try(keys(local.configs["audit-archiver"]), []), ["disk", "s3", "splunk", "datadog"])) > 0
      error_message = "Turn on at least one audit-archiver sink."
    }
    precondition {
      condition     = each.key != "proposal-chatops" || try(coalesce(local.configs["proposal-chatops"].slack.channel), "") != ""
      error_message = "The proposal-chatops configuration needs slack.channel."
    }
    precondition {
      condition     = each.key != "proposal-chatops" || alltrue([for k in ["SLACK_BOT_TOKEN", "SLACK_SIGNING_SECRET"] : lookup(local.secrets, k, "") != ""])
      error_message = "proposal-chatops needs the SLACK_BOT_TOKEN and SLACK_SIGNING_SECRET secrets."
    }
    precondition {
      condition     = each.key != "router" || length(local.router_event_types) > 0
      error_message = "router needs at least one route with events."
    }
    precondition {
      condition     = alltrue([for f, content in each.value.files : can(yamldecode(content))])
      error_message = "The configuration of ${each.key} is not valid YAML."
    }
  }
}
