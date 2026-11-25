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
      condition     = each.key != "audit-archiver" || length(setintersection(keys(local.settings[each.key]), ["ARCHIVE_S3_BUCKET", "SPLUNK_HEC_URL", "DD_API_KEY"])) > 0
      error_message = "audit-archiver needs an S3 bucket, a Splunk HEC URL, or the AUDIT_ARCHIVER_DD_API_KEY secret."
    }
    precondition {
      condition     = each.key != "proposal-chatops" || contains(keys(local.settings[each.key]), "SLACK_CHANNEL")
      error_message = "proposal-chatops needs a channel."
    }
    precondition {
      condition     = alltrue([for f, content in each.value.files : can(yamldecode(content))])
      error_message = "The YAML configuration of ${each.key} is not valid YAML."
    }
  }
}
