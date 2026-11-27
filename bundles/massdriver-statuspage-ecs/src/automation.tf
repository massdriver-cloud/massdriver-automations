locals {
  environments = distinct(flatten([
    for page in coalesce(var.config.pages, []) : [for env in coalesce(page.environments, []) : env.id]
  ]))
  # jq has no empty IN(), so an empty list tests false.
  environment_test = (length(local.environments) > 0
    ? "($env | IN(${join(", ", [for e in local.environments : jsonencode(e)])}))"
  : "false")
  # The stash event types are left out when the config reads no stash key, so stash-sync's writes do not reach it.
  stash       = try(var.config.stash.health == true || var.config.stash.banners == true, false)
  event_types = concat(["alarm.*", "alarm_state.created", "deployment.started", "deployment.completed", "deployment.failed", "deployment.aborted", "instance.*"], local.stash ? ["stash_entry.set", "stash_entry.deleted"] : [])

  automation = {
    name        = "statuspage"
    image       = local.image
    files       = { (local.config_file) = yamlencode(var.config) }
    state       = true
    env         = merge(local.massdriver_env, { STATE_DIR = local.state_dir })
    secrets     = local.massdriver_secrets
    event_types = local.event_types
    attributes  = coalesce(var.attributes, {})
    condition   = <<-JQ
      (.data.attributes["md-project"] + "-" + .data.attributes["md-environment"]) as $env
      | ${local.environment_test}
        and (.data.metadata.deployment_action // "PROVISION") != "PLAN"
        and ((.type | startswith("stash_entry.") | not) or (.data.entity.key | IN("md/v1/health", "md/v1/banner")))
    JQ
    # statuspage writes the static site to output_dir, when its config sets one.
    site_dir = try(trimsuffix(coalesce(var.config.output_dir), "/"), "")
  }
}

resource "massdriver_resource" "website" {
  field = "website"
  name  = "${local.name_prefix} status page"
  resource = jsonencode({
    url = "https://${local.host}/"
  })
}
