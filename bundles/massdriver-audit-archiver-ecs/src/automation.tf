locals {
  # Each sink that is on becomes a section of the config file.
  config = merge(
    {
      for sink in ["disk", "splunk", "datadog"] : sink => {
        for k, v in var.config[sink] : k => v if k != "enabled" && v != null
      } if try(var.config[sink].enabled, false)
    },
    try(var.config.s3.enabled, false) ? {
      s3 = merge(
        {
          for k, v in var.config.s3 : k => v
          if v != null && !contains(["enabled", "object_lock_days", "object_lock_mode"], k)
        },
        coalesce(var.config.s3.object_lock_days, 0) > 0 ? {
          object_lock = {
            mode = coalesce(var.config.s3.object_lock_mode, "COMPLIANCE")
            days = var.config.s3.object_lock_days
          }
        } : null,
      )
    } : null,
  )

  s3        = try(local.config.s3, null)
  s3_bucket = try(coalesce(local.s3.bucket), "")
  # An S3-compatible store or static keys need no IAM role.
  s3_role = (local.s3_bucket != "" && try(coalesce(local.s3.endpoint), "") == ""
  && lookup(local.secrets, "AWS_ACCESS_KEY_ID", "") == "")

  automation = {
    name  = "audit-archiver"
    image = local.image
    files = { (local.config_file) = yamlencode(local.config) }
    secrets = {
      AWS_ACCESS_KEY_ID     = lookup(local.secrets, "AWS_ACCESS_KEY_ID", "")
      AWS_SECRET_ACCESS_KEY = lookup(local.secrets, "AWS_SECRET_ACCESS_KEY", "")
      SPLUNK_HEC_TOKEN      = lookup(local.secrets, "SPLUNK_HEC_TOKEN", "")
      DD_API_KEY            = lookup(local.secrets, "DD_API_KEY", "")
    }
    # An attribute filter would drop events without attributes, such as service account events.
    event_types = []
    s3_write = local.s3_role ? {
      bucket      = local.s3_bucket
      prefix      = try(coalesce(trim(local.s3.prefix, "/")), "events")
      object_lock = try(local.s3.object_lock, null) != null
    } : null
  }
}

resource "terraform_data" "checks" {
  lifecycle {
    precondition {
      condition     = length(keys(local.config)) > 0
      error_message = "Turn on at least one sink."
    }
  }
}
