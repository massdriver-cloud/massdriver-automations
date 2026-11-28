locals {
  automation = {
    name    = "scorecards"
    image   = local.image
    files   = { (local.config_file) = var.config }
    env     = local.massdriver_env
    secrets = local.massdriver_secrets
    event_types = [
      "deployment.completed", "deployment.failed", "deployment.aborted", "deployment.rejected",
      "alarm_state.created", "alarm.created", "alarm.updated", "alarm.deleted",
      "instance.created", "instance.updated", "instance.deleted", "instance.moved", "instance.orphaned",
      "instance.copied",
      "repo.published",
      "group_membership.added", "group_membership.removed",
      "stash_entry.set", "stash_entry.deleted",
    ]
    # An attribute filter would drop events without attributes, such as group membership and repo events.
    attributes = {}
    # Drop the stash events of the keys scorecards writes, so its own writes do not start more work.
    condition = "(.type | startswith(\"stash_entry.\") | not) or (.data.entity.key | test(\"^md/v1/(scorecard/|(delivery|reliability|currency|pending|parity|ownership)$)\") | not)"
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
