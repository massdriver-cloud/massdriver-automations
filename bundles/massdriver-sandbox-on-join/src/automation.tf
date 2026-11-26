locals {
  groups = distinct([for rule in coalesce(var.config.sandboxes, []) : rule.group])
  # A rule names its group by name or by ID. jq has no empty IN(), so an empty list tests false.
  group_list = join(", ", [for g in local.groups : jsonencode(g)])
  group_test = (length(local.groups) > 0
    ? "((.data.entity.group_name | IN(${local.group_list})) or (.data.entity.group_id | IN(${local.group_list})))"
  : "false")

  automation = {
    name        = "sandbox-on-join"
    image       = local.image
    files       = { (local.config_file) = yamlencode(var.config) }
    env         = local.massdriver_env
    secrets     = local.massdriver_secrets
    event_types = ["group_membership.added", "group_membership.removed", "deployment.completed"]
    # Group membership events have no attributes, so an attribute filter would drop them.
    condition = join(" ", [
      "((.type | startswith(\"group_membership.\")) and .data.entity.account_id != null",
      "and ${local.group_test})",
      "or (.type == \"deployment.completed\" and .data.metadata.deployment_action == \"DECOMMISSION\"",
      "and .data.attributes.sandbox == \"true\")",
    ])
  }
}
