locals {
  # Bundle secrets are not OpenTofu variables. The provisioner writes them to this file.
  secrets = try(jsondecode(file("/massdriver/secrets.json")), {})

  namespace   = coalesce(var.kubernetes_cluster.namespace, "default")
  name_prefix = var.md_metadata.name_prefix
  config_dir  = "/etc/massdriver-automations"
  state_dir   = "/var/lib/statuspage"

  image = "${var.image.repository}:${var.image.tag}"

  massdriver_env = {
    MASSDRIVER_ORGANIZATION_ID = var.massdriver.organization_id
    MASSDRIVER_URL             = var.massdriver.url
    MASSDRIVER_API_KEY         = lookup(local.secrets, "MASSDRIVER_API_KEY", null)
  }

  # The secret may arrive as a JSON string or as an object.
  router_env = try(jsondecode(local.secrets.ROUTER_ENV), lookup(local.secrets, "ROUTER_ENV", {}))

  statuspage_config = try(yamldecode(var.statuspage.config), {})
  sandbox_config    = try(yamldecode(var.sandbox_on_join.config), {})
  router_config     = try(yamldecode(var.router.config), {})

  router_routes      = try(coalesce(local.router_config.routes), [])
  router_event_types = distinct(flatten([for r in local.router_routes : try(coalesce(r.events), [])]))
  # Commands need a shell, jq, curl, or python3, which only the -scripts image has.
  router_uses_exec = length(var.router.scripts) > 0 || anytrue(flatten([
    for r in local.router_routes : [for s in try(coalesce(r.do), []) : try(s.exec, null) != null]
  ]))
  scripts_dir = "/scripts"


  statuspage_environments = distinct(flatten([
    for page in try(coalesce(local.statuspage_config.pages), []) : [
      for env in try(coalesce(page.environments), []) : env.id
    ]
  ]))

  sandbox_groups = distinct([for rule in try(coalesce(local.sandbox_config.sandboxes), []) : rule.group])

  # jq has no empty IN(), so an empty list tests false.
  sandbox_group_test = (length(local.sandbox_groups) > 0
    ? "(.data.entity.group_name | IN(${join(", ", [for g in local.sandbox_groups : jsonencode(g)])}))"
  : "false")
  statuspage_environment_test = (length(local.statuspage_environments) > 0
    ? "($env | IN(${join(", ", [for e in local.statuspage_environments : jsonencode(e)])}))"
  : "false")

  statuspage_host = coalesce(var.statuspage.host, "statuspage.${var.ingress.domain}")

  ingress_class   = try(coalesce(var.ingress.class_name), null)
  cluster_issuer  = try(coalesce(var.ingress.cluster_issuer), "")
  tls_secret_name = try(coalesce(var.ingress.tls_secret_name), "")

  automations = {
    audit-archiver = {
      enabled    = var.audit_archiver.enabled
      image      = local.image
      host       = "audit-archiver.${var.ingress.domain}"
      files      = {}
      state_size = 0
      env = {
        ARCHIVE_S3_BUCKET           = var.audit_archiver.s3_bucket
        ARCHIVE_S3_PREFIX           = var.audit_archiver.s3_prefix
        ARCHIVE_S3_ENDPOINT         = var.audit_archiver.s3_endpoint
        ARCHIVE_S3_OBJECT_LOCK_MODE = var.audit_archiver.s3_object_lock_mode
        ARCHIVE_S3_OBJECT_LOCK_DAYS = var.audit_archiver.s3_object_lock_days > 0 ? tostring(var.audit_archiver.s3_object_lock_days) : null
        AWS_REGION                  = var.audit_archiver.s3_region
        AWS_ACCESS_KEY_ID           = lookup(local.secrets, "AUDIT_ARCHIVER_AWS_ACCESS_KEY_ID", "")
        AWS_SECRET_ACCESS_KEY       = lookup(local.secrets, "AUDIT_ARCHIVER_AWS_SECRET_ACCESS_KEY", "")
        SPLUNK_HEC_URL              = var.audit_archiver.splunk_hec_url
        SPLUNK_HEC_TOKEN            = lookup(local.secrets, "AUDIT_ARCHIVER_SPLUNK_HEC_TOKEN", "")
        SPLUNK_INDEX                = var.audit_archiver.splunk_index
        DD_API_KEY                  = lookup(local.secrets, "AUDIT_ARCHIVER_DD_API_KEY", "")
        DD_SITE                     = var.audit_archiver.datadog_site
        DD_TAGS                     = var.audit_archiver.datadog_tags
      }
      # An attribute filter would drop events without attributes, such as service account events.
      event_types = []
      attributes  = {}
      condition   = ""
    }

    autodeploy-downstream = {
      enabled    = var.autodeploy_downstream.enabled
      image      = local.image
      host       = "autodeploy-downstream.${var.ingress.domain}"
      files      = {}
      state_size = 0
      env = merge(local.massdriver_env, {
        AUTO_DEPLOY_ATTR_KEY   = var.autodeploy_downstream.attribute_key
        AUTO_DEPLOY_ATTR_VALUE = var.autodeploy_downstream.attribute_value
      })
      event_types = ["deployment.completed"]
      attributes  = var.autodeploy_downstream.attributes
      condition   = ".data.metadata.deployment_action == \"PROVISION\""
    }

    bundle-scanner = {
      enabled    = var.bundle_scanner.enabled
      image      = "${local.image}-scanner"
      host       = "bundle-scanner.${var.ingress.domain}"
      files      = {}
      state_size = 0
      env = merge(local.massdriver_env, {
        SLACK_WEBHOOK_URL  = lookup(local.secrets, "BUNDLE_SCANNER_SLACK_WEBHOOK_URL", "")
        SCANNERS           = var.bundle_scanner.scanners
        SCAN_FAIL_SEVERITY = var.bundle_scanner.fail_severity
        SCAN_ATTRIBUTE_KEY = var.bundle_scanner.result_attribute_key
        SCAN_TIMEOUT       = var.bundle_scanner.timeout
      })
      event_types = ["repo.published"]
      attributes  = var.bundle_scanner.attributes
      condition   = ".data.entity.artifact_type == \"application/vnd.massdriver.bundle.v1+json\""
    }

    drift = {
      enabled    = var.drift.enabled
      image      = local.image
      host       = "drift.${var.ingress.domain}"
      files      = { "drift.yaml" = var.drift.config }
      state_size = var.drift.storage_size_gi
      env = merge(local.massdriver_env, {
        DRIFT_CONFIG       = "${local.config_dir}/drift.yaml"
        STATE_DIR          = local.state_dir
        MASSDRIVER_APP_URL = var.massdriver.app_url
        SLACK_WEBHOOK_URL  = lookup(local.secrets, "DRIFT_SLACK_WEBHOOK_URL", "")
        DRIFT_RUN_TOKEN    = lookup(local.secrets, "DRIFT_RUN_TOKEN", "")
      })
      event_types = ["deployment.completed", "deployment.failed", "deployment.aborted"]
      attributes  = var.drift.attributes
      condition   = ".data.metadata.deployment_action == \"PLAN\""
    }

    freeze-guard = {
      enabled    = var.freeze_guard.enabled
      image      = local.image
      host       = "freeze-guard.${var.ingress.domain}"
      files      = { "windows.yaml" = var.freeze_guard.windows }
      state_size = 0
      env = merge(local.massdriver_env, {
        FREEZE_WINDOWS_FILE   = "${local.config_dir}/windows.yaml"
        FREEZE_OVERRIDE_TOKEN = var.freeze_guard.override_token
        SLACK_WEBHOOK_URL     = lookup(local.secrets, "FREEZE_GUARD_SLACK_WEBHOOK_URL", "")
      })
      event_types = ["deployment.created", "deployment.enqueued"]
      attributes  = var.freeze_guard.attributes
      condition   = ".data.metadata.deployment_action != \"PLAN\" and (.data.entity.status == \"PENDING\" or .data.entity.status == \"APPROVED\")"
    }

    github-deployments = {
      enabled    = var.github_deployments.enabled
      image      = local.image
      host       = "github-deployments.${var.ingress.domain}"
      files      = {}
      state_size = 0
      env = {
        GITHUB_TOKEN            = lookup(local.secrets, "GITHUB_DEPLOYMENTS_TOKEN", "")
        GITHUB_API_URL          = var.github_deployments.api_url
        MASSDRIVER_APP_URL      = var.massdriver.app_url
        GITHUB_REPO_ATTR_KEY    = var.github_deployments.repo_attribute_key
        GITHUB_REF_ATTR_KEY     = var.github_deployments.ref_attribute_key
        GITHUB_ENVIRONMENT_NAME = var.github_deployments.environment_name
      }
      event_types = ["deployment.started", "deployment.completed", "deployment.failed", "deployment.aborted"]
      attributes  = {}
      condition   = ".data.metadata.deployment_action != \"PLAN\" and (.data.attributes[${jsonencode(var.github_deployments.repo_attribute_key)}] // \"\") != \"\""
    }

    proposal-chatops = {
      enabled    = var.proposal_chatops.enabled
      image      = local.image
      host       = "proposal-chatops.${var.ingress.domain}"
      files      = {}
      state_size = 0
      env = merge(local.massdriver_env, {
        SLACK_BOT_TOKEN        = lookup(local.secrets, "PROPOSAL_CHATOPS_SLACK_BOT_TOKEN", "")
        SLACK_SIGNING_SECRET   = lookup(local.secrets, "PROPOSAL_CHATOPS_SLACK_SIGNING_SECRET", "")
        SLACK_CHANNEL          = var.proposal_chatops.channel
        SLACK_CHANNEL_ATTR_KEY = var.proposal_chatops.channel_attribute_key
        SLACK_ALLOWED_USERS    = var.proposal_chatops.allowed_users
      })
      event_types = ["deployment.proposed", "deployment.approved", "deployment.rejected"]
      attributes  = var.proposal_chatops.attributes
      condition   = ""
    }

    release-notes = {
      enabled    = var.release_notes.enabled
      image      = local.image
      host       = "release-notes.${var.ingress.domain}"
      files      = {}
      state_size = 0
      env = merge(local.massdriver_env, {
        SLACK_WEBHOOK_URL   = lookup(local.secrets, "RELEASE_NOTES_SLACK_WEBHOOK_URL", "")
        TEAMS_WEBHOOK_URL   = lookup(local.secrets, "RELEASE_NOTES_TEAMS_WEBHOOK_URL", "")
        INCLUDE_DEVELOPMENT = tostring(var.release_notes.include_development)
      })
      event_types = ["repo.published"]
      attributes  = { "md-repo-artifact-type" = "bundle" }
      condition   = ""
    }

    rollback-on-failure = {
      enabled    = var.rollback_on_failure.enabled
      image      = local.image
      host       = "rollback-on-failure.${var.ingress.domain}"
      files      = {}
      state_size = 0
      env = merge(local.massdriver_env, {
        AUTO_ROLLBACK_ATTR_KEY   = var.rollback_on_failure.attribute_key
        AUTO_ROLLBACK_ATTR_VALUE = var.rollback_on_failure.attribute_value
        MASSDRIVER_APP_URL       = var.massdriver.app_url
        SLACK_WEBHOOK_URL        = lookup(local.secrets, "ROLLBACK_ON_FAILURE_SLACK_WEBHOOK_URL", "")
      })
      event_types = ["deployment.failed"]
      attributes  = var.rollback_on_failure.attributes
      condition   = ".data.metadata.deployment_action == \"PROVISION\" and .data.attributes[${jsonencode(var.rollback_on_failure.attribute_key)}] == ${jsonencode(var.rollback_on_failure.attribute_value)}"
    }

    router = {
      enabled    = var.router.enabled
      image      = local.router_uses_exec ? "${local.image}-scripts" : local.image
      host       = "router.${var.ingress.domain}"
      files      = { "config.yaml" = var.router.config }
      state_size = 0
      env = merge(local.router_env, local.massdriver_env, {
        ROUTER_CONFIG = "${local.config_dir}/config.yaml"
      })
      event_types = local.router_event_types
      # Routes filter on attributes and jq themselves, and an attribute filter would drop events without attributes.
      attributes = {}
      condition  = ""
    }

    sandbox-on-join = {
      enabled    = var.sandbox_on_join.enabled
      image      = local.image
      host       = "sandbox-on-join.${var.ingress.domain}"
      files      = { "sandboxes.yaml" = var.sandbox_on_join.config }
      state_size = 0
      env = merge(local.massdriver_env, {
        SANDBOX_CONFIG = "${local.config_dir}/sandboxes.yaml"
      })
      event_types = ["group_membership.added", "group_membership.removed", "deployment.completed"]
      # Group membership events have no attributes, so an attribute filter would drop them.
      attributes = {}
      condition = join(" ", [
        "((.type | startswith(\"group_membership.\")) and .data.entity.account_id != null",
        "and ${local.sandbox_group_test})",
        "or (.type == \"deployment.completed\" and .data.metadata.deployment_action == \"DECOMMISSION\"",
        "and .data.attributes.sandbox == \"true\")",
      ])
    }

    statuspage = {
      enabled    = var.statuspage.enabled
      image      = local.image
      host       = local.statuspage_host
      files      = { "statuspage.yaml" = var.statuspage.config }
      state_size = var.statuspage.storage_size_gi
      env = merge(local.massdriver_env, {
        STATUSPAGE_CONFIG = "${local.config_dir}/statuspage.yaml"
        STATE_DIR         = local.state_dir
      })
      event_types = [
        "alarm.*", "alarm_state.created", "deployment.started", "deployment.completed", "deployment.failed",
        "deployment.aborted", "instance.*",
      ]
      attributes = var.statuspage.attributes
      condition  = <<-JQ
        (.data.attributes["md-project"] + "-" + .data.attributes["md-environment"]) as $env
        | ${local.statuspage_environment_test}
          and (.data.metadata.deployment_action // "PROVISION") != "PLAN"
      JQ
    }
  }

  enabled = { for name, a in local.automations : name => a if a.enabled }

  # Unset settings are left out, so each automation uses its own default.
  settings = {
    for name, a in local.enabled : name => { for k, v in a.env : k => tostring(v) if v != null && v != "" }
  }

  env = {
    for name, a in local.enabled : name => merge(
      local.settings[name],
      { MASSDRIVER_SIGNING_SECRET = massdriver_event_destination.main[name].signing_secret }
    )
  }

  labels = {
    "app.kubernetes.io/part-of"    = "massdriver-automations"
    "app.kubernetes.io/managed-by" = "massdriver"
    "app.kubernetes.io/instance"   = local.name_prefix
  }

  # Kubernetes names are at most 63 characters, and the longest automation name is 21.
  names = { for name, _ in local.automations : name => "${trimsuffix(substr(local.name_prefix, 0, 41), "-")}-${name}" }
}
