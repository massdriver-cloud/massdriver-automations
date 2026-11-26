locals {
  # The routes are free-form, so the form holds the YAML of the config file.
  routes      = try(coalesce(yamldecode(var.config).routes), [])
  event_types = distinct(flatten([for r in local.routes : try(coalesce(r.events), [])]))
  scripts     = coalesce(var.scripts, {})
  # Commands need a shell, jq, curl, or python3, which only the -scripts image has.
  uses_exec = length(local.scripts) > 0 || anytrue(flatten([
    for r in local.routes : [for s in try(coalesce(r.do), []) : try(s.exec, null) != null]
  ]))
  # The secret may arrive as a JSON string or as an object.
  router_env = try(jsondecode(local.secrets.ROUTER_ENV), lookup(local.secrets, "ROUTER_ENV", {}))

  automation = {
    name        = "router"
    image       = local.uses_exec ? "${local.image}-scripts" : local.image
    files       = { (local.config_file) = var.config }
    env         = local.massdriver_env
    secrets     = merge(local.router_env, local.massdriver_secrets)
    event_types = local.event_types
    scripts     = local.scripts
  }
}

resource "terraform_data" "checks" {
  lifecycle {
    precondition {
      condition     = can(yamldecode(var.config))
      error_message = "The routes are not valid YAML."
    }
    precondition {
      condition     = length(local.event_types) > 0
      error_message = "Add at least one route with events."
    }
  }
}
