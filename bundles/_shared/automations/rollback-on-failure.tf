locals {
  # The same defaults as the automation, so the destination sends what it acts on.
  attribute_key   = try(coalesce(var.config.attribute.key), "autorollback")
  attribute_value = try(tostring(coalesce(var.config.attribute.value)), "true")

  automation = {
    name        = "rollback-on-failure"
    image       = local.image
    files       = { (local.config_file) = yamlencode(var.config) }
    env         = local.massdriver_env
    secrets     = merge(local.massdriver_secrets, local.slack_secrets)
    event_types = ["deployment.failed", "deployment.completed"]
    attributes  = coalesce(var.attributes, {})
    # A completed deployment matters only when it is a rollback, whose result goes in the notice's thread.
    condition = join(" ", [
      ".data.metadata.deployment_action == \"PROVISION\"",
      "and .data.attributes[${jsonencode(local.attribute_key)}] == ${jsonencode(local.attribute_value)}",
      "and (.type == \"deployment.failed\" or ((.data.entity.message // \"\") | startswith(\"Rollback to deployment \")))",
    ])
  }
}
