locals {
  # Bundle secrets are not OpenTofu variables. The provisioner writes them to this file.
  secrets = try(jsondecode(file("/massdriver/secrets.json")), {})

  name_prefix = var.md_metadata.name_prefix
  config_dir  = "/etc/massdriver-automations"
  state_dir   = "/var/lib/massdriver-automations"
  scripts_dir = "/scripts"
  config_file = "config.yaml"

  image = "${var.image.repository}:${var.image.tag}"
  # A tag such as latest or 0.2 moves to new images. A deploy restarts the automation so it runs the image the tag
  # names now, the same as a deploy of a full version such as 0.2.1.
  image_moves = !can(regex("^v?[0-9]+\\.[0-9]+\\.[0-9]+$", var.image.tag))

  zone_id     = try(var.ecs_cluster.zone.zone_id, "")
  zone_domain = try(var.ecs_cluster.zone.domain, "")
  host        = try(coalesce(var.ingress.host, local.zone_domain != "" ? "${local.automation.name}.${local.zone_domain}" : null), "")

  massdriver_env = {
    MASSDRIVER_ORGANIZATION_ID = var.service_account.organization_id
    MASSDRIVER_URL             = var.service_account.url
  }
  massdriver_secrets = {
    MASSDRIVER_API_KEY = var.service_account.api_key
  }
  # One Slack app serves every automation that posts.
  slack_secrets = {
    SLACK_BOT_TOKEN   = lookup(local.secrets, "SLACK_BOT_TOKEN", "")
    SLACK_WEBHOOK_URL = lookup(local.secrets, "SLACK_WEBHOOK_URL", "")
  }

  enabled = {
    (local.automation.name) = merge({
      host       = local.host
      state      = false
      env        = {}
      secrets    = {}
      attributes = {}
      condition  = ""
      scripts    = {}
      site_dir   = ""
      large      = false
      # An S3 bucket that the task role writes to: { bucket, prefix, object_lock }.
      s3_write = null
      # The task role reads CloudWatch metrics and alarms.
      cloudwatch_read = false
    }, local.automation)
  }
  stateful = { for name, a in local.enabled : name => a if a.state }

  # Unset settings are left out, so the automation uses its own default.
  settings = {
    for name, a in local.enabled : name => {
      for k, v in merge(a.env, { OTEL_EXPORTER_OTLP_ENDPOINT = try(var.otel.endpoint, null) }) :
      k => tostring(v) if v != null && v != ""
    }
  }

  secret_env = {
    for name, a in local.enabled : name => merge(
      {
        for k, v in merge(a.secrets, { OTEL_EXPORTER_OTLP_HEADERS = lookup(local.secrets, "OTEL_EXPORTER_OTLP_HEADERS", "") }) :
        k => tostring(v) if v != null && v != ""
      },
      { MASSDRIVER_SIGNING_SECRET = massdriver_event_destination.main[name].signing_secret }
    )
  }

  # Written to the task's volumes by the init container, because the image has no shell and Fargate has no
  # equivalent of a ConfigMap.
  files = {
    for name, a in local.enabled : name => concat(
      [for f, content in a.files : { path = "${local.config_dir}/${f}", mode = "0644", content = content }],
      [for f, content in a.scripts : { path = "${local.scripts_dir}/${f}", mode = "0755", content = content }],
    )
  }

  # ECS, IAM, and target group names have different length limits. The longest automation name is 21 characters.
  names      = { for name, _ in local.enabled : name => "${trimsuffix(substr(local.name_prefix, 0, 41), "-")}-${name}" }
  role_names = { for name, _ in local.enabled : name => "${trimsuffix(substr(local.name_prefix, 0, 36), "-")}-${name}" }
  target_group_names = {
    for name, _ in local.enabled : name => "${substr(sha1("${local.name_prefix}-${name}"), 0, 10)}-${name}"
  }

  dns_hosts = {
    for name, a in local.enabled : name => a.host
    if var.ingress.dns_records && local.zone_id != "" && endswith(a.host, ".${local.zone_domain}")
  }
}

check "host" {
  assert {
    condition     = local.host != ""
    error_message = "Set Ingress > Host, or connect a cluster with a hosted zone."
  }
}
