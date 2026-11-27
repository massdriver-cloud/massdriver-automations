locals {
  # Bundle secrets are not OpenTofu variables. The provisioner writes them to this file.
  secrets = try(jsondecode(file("/massdriver/secrets.json")), {})

  namespace   = coalesce(try(var.kubernetes_cluster.namespace, null), "default")
  name_prefix = var.md_metadata.name_prefix
  config_dir  = "/etc/massdriver-automations"
  state_dir   = "/var/lib/massdriver-automations"
  scripts_dir = "/scripts"
  config_file = "config.yaml"

  image = "${var.image.repository}:${var.image.tag}"
  # A tag such as latest or 0.2 moves to new images. A deploy restarts the automation so it runs the image the tag
  # names now, the same as a deploy of a full version such as 0.2.1.
  image_moves = !can(regex("^v?[0-9]+\\.[0-9]+\\.[0-9]+$", var.image.tag))
  host        = var.ingress.host

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
    }, local.automation)
  }

  # Unset settings are left out, so the automation uses its own default.
  env = {
    for name, a in local.enabled : name => merge(
      {
        for k, v in merge(a.env, a.secrets, {
          OTEL_EXPORTER_OTLP_ENDPOINT = try(var.otel.endpoint, null)
          OTEL_EXPORTER_OTLP_HEADERS  = lookup(local.secrets, "OTEL_EXPORTER_OTLP_HEADERS", "")
        }) : k => tostring(v) if v != null && v != ""
      },
      { MASSDRIVER_SIGNING_SECRET = massdriver_event_destination.main[name].signing_secret }
    )
  }

  labels = {
    "app.kubernetes.io/part-of"    = "massdriver-automations"
    "app.kubernetes.io/managed-by" = "massdriver"
    "app.kubernetes.io/instance"   = local.name_prefix
  }

  # Kubernetes names are at most 63 characters.
  names = { for name, _ in local.enabled : name => trimsuffix(substr(local.name_prefix, 0, 52), "-") }

  ingress_class   = try(coalesce(var.ingress.class_name), null)
  cluster_issuer  = try(coalesce(var.ingress.cluster_issuer), "")
  tls_secret_name = try(coalesce(var.ingress.tls_secret_name), "")
}
