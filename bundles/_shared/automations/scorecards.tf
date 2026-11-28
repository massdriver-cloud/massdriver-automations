locals {
  scorecards_config = try(yamldecode(var.config), {})
  prefix            = trimsuffix(try(coalesce(local.scorecards_config.results.prefix), "md/v1"), "/")
  # A check's stash is a key or {key, scope}.
  check_keys = distinct(compact(flatten([
    for card in try(local.scorecards_config.scorecards, []) : [for check in try(card.checks, []) : try(check.stash.key, check.stash, "")]
  ])))
  # Only the stash keys that checks read start a computation, so other automations' frequent writes are not sent.
  stash_events = length(local.check_keys) > 0 ? ["stash_entry.set", "stash_entry.deleted"] : []

  automation = {
    name  = "scorecards"
    image = local.image
    # Checks are jq expressions, so the form holds the YAML of the config file.
    files   = { (local.config_file) = var.config }
    env     = local.massdriver_env
    secrets = local.massdriver_secrets
    event_types = concat([
      "deployment.completed", "deployment.failed", "deployment.aborted", "deployment.rejected",
      "alarm.*", "alarm_state.created", "instance.*", "repo.published", "group_membership.*",
    ], local.stash_events)
    attributes = {}
    condition  = length(local.check_keys) == 0 ? "true" : <<-JQ
      (.type | startswith("stash_entry.") | not)
        or ((.data.entity.key | IN(${join(", ", [for k in local.check_keys : jsonencode(k)])}))
          and (.data.entity.key | startswith(${jsonencode("${local.prefix}/scorecard/")}) | not))
    JQ
  }
}

resource "terraform_data" "checks" {
  lifecycle {
    precondition {
      condition     = can(yamldecode(var.config))
      error_message = "The settings are not valid YAML."
    }
  }
}
