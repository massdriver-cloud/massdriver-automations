locals {
  # Attribute values with commas become lists, which match any of their values.
  config = {
    rules = [
      for r in coalesce(var.config.rules, []) : {
        name       = r.name
        action     = r.action
        downstream = { for k, v in r.downstream : k => [for x in split(",", v) : trimspace(x)] }
        upstream   = { for k, v in coalesce(r.upstream, {}) : k => [for x in split(",", v) : trimspace(x)] }
      }
    ]
  }

  automation = {
    name        = "autodeploy-downstream"
    image       = local.image
    files       = { (local.config_file) = yamlencode(local.config) }
    env         = local.massdriver_env
    secrets     = local.massdriver_secrets
    event_types = ["deployment.completed"]
    attributes  = coalesce(var.attributes, {})
    # The rules' upstream attributes are left to the automation, which checks them on each event.
    condition = ".data.metadata.deployment_action == \"PROVISION\""
  }
}
