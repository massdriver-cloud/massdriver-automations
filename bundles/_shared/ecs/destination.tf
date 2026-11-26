resource "massdriver_event_destination" "main" {
  for_each = local.enabled

  name    = local.name_prefix
  adapter = "HTTP"
  config = jsonencode({
    url = "https://${each.value.host}/hooks/massdriver"
  })

  event_types = each.value.event_types
  attributes  = each.value.attributes
  condition   = trimspace(each.value.condition) != "" ? trimspace(each.value.condition) : null
}
