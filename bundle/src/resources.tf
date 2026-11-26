# Each enabled automation publishes its event destination as a resource, so other bundles and the Massdriver UI can
# find the URL and the signing secret.
resource "massdriver_resource" "destination" {
  for_each = massdriver_event_destination.main

  field = "${replace(each.key, "-", "_")}_destination"
  name  = "${local.name_prefix} ${each.key}"
  resource = jsonencode(merge(
    {
      id             = each.value.id
      name           = each.value.name
      url            = "https://${local.enabled[each.key].host}/hooks/massdriver"
      signing_secret = each.value.signing_secret
      event_types    = tolist(coalesce(each.value.event_types, []))
      attributes     = coalesce(each.value.attributes, {})
    },
    each.value.condition == null ? {} : { condition = each.value.condition },
  ))
}
