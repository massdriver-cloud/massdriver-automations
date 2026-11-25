locals {
  # Bundle secrets are not OpenTofu variables. The provisioner writes them to this file.
  secrets = try(jsondecode(file("/massdriver/secrets.json")), {})

  name_prefix = var.md_metadata.name_prefix
  config_dir  = "/etc/massdriver-automations"
  state_dir   = "/var/lib/statuspage"
  scripts_dir = "/scripts"

  image = "${var.image.repository}:${var.image.tag}"

  domain = try(coalesce(var.ingress.domain, try(var.ecs_cluster.dns.base_domain, null)), "")

  massdriver_env = {
    MASSDRIVER_ORGANIZATION_ID = var.service_account.organization_id
    MASSDRIVER_URL             = var.service_account.url
  }
  massdriver_secrets = {
    MASSDRIVER_API_KEY = var.service_account.api_key
  }

  # The secret may arrive as a JSON string or as an object.
  router_env = try(jsondecode(local.secrets.ROUTER_ENV), lookup(local.secrets, "ROUTER_ENV", {}))

  statuspage_config = try(yamldecode(var.statuspage.config), {})
  sandbox_config    = try(yamldecode(var.sandbox_on_join.config), {})
  router_config     = try(yamldecode(var.router.config), {})

  autodeploy_rules = trimspace(var.autodeploy_downstream.config) != ""

  router_routes      = try(coalesce(local.router_config.routes), [])
  router_event_types = distinct(flatten([for r in local.router_routes : try(coalesce(r.events), [])]))
  # Commands need a shell, jq, curl, or python3, which only the -scripts image has.
  router_uses_exec = length(var.router.scripts) > 0 || anytrue(flatten([
    for r in local.router_routes : [for s in try(coalesce(r.do), []) : try(s.exec, null) != null]
  ]))

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

  statuspage_host = coalesce(var.statuspage.host, "statuspage.${local.domain}")

  audit_archiver_static_keys = lookup(local.secrets, "AUDIT_ARCHIVER_AWS_ACCESS_KEY_ID", "") != ""
  # An S3-compatible store or static keys need no IAM; otherwise the task role writes to the bucket.
  audit_archiver_task_role_s3 = (
    try(coalesce(var.audit_archiver.s3_bucket), "") != ""
    && try(coalesce(var.audit_archiver.s3_endpoint), "") == ""
    && !local.audit_archiver_static_keys
  )
  audit_archiver_s3_prefix = coalesce(trim(var.audit_archiver.s3_prefix, "/"), "events")

  automations = {
    audit-archiver = {
      enabled = var.audit_archiver.enabled
      image   = local.image
      host    = "audit-archiver.${local.domain}"
      files   = {}
      state   = false
      env = {
        ARCHIVE_S3_BUCKET           = var.audit_archiver.s3_bucket
        ARCHIVE_S3_PREFIX           = var.audit_archiver.s3_prefix
        ARCHIVE_S3_ENDPOINT         = var.audit_archiver.s3_endpoint
        ARCHIVE_S3_OBJECT_LOCK_MODE = var.audit_archiver.s3_object_lock_mode
        ARCHIVE_S3_OBJECT_LOCK_DAYS = var.audit_archiver.s3_object_lock_days > 0 ? tostring(var.audit_archiver.s3_object_lock_days) : null
        AWS_REGION                  = var.audit_archiver.s3_region
        SPLUNK_HEC_URL              = var.audit_archiver.splunk_hec_url
        SPLUNK_INDEX                = var.audit_archiver.splunk_index
        DD_SITE                     = var.audit_archiver.datadog_site
        DD_TAGS                     = var.audit_archiver.datadog_tags
      }
      secrets = {
        AWS_ACCESS_KEY_ID     = lookup(local.secrets, "AUDIT_ARCHIVER_AWS_ACCESS_KEY_ID", "")
        AWS_SECRET_ACCESS_KEY = lookup(local.secrets, "AUDIT_ARCHIVER_AWS_SECRET_ACCESS_KEY", "")
        SPLUNK_HEC_TOKEN      = lookup(local.secrets, "AUDIT_ARCHIVER_SPLUNK_HEC_TOKEN", "")
        DD_API_KEY            = lookup(local.secrets, "AUDIT_ARCHIVER_DD_API_KEY", "")
      }
      # An attribute filter would drop events without attributes, such as service account events.
      event_types = []
      attributes  = {}
      condition   = ""
    }

    autodeploy-downstream = {
      enabled = var.autodeploy_downstream.enabled
      image   = local.image
      host    = "autodeploy-downstream.${local.domain}"
      files   = local.autodeploy_rules ? { "autodeploy.yaml" = var.autodeploy_downstream.config } : {}
      state   = false
      # The automation refuses to start with both a rules file and the attribute variables.
      env = merge(local.massdriver_env, {
        AUTODEPLOY_CONFIG      = local.autodeploy_rules ? "${local.config_dir}/autodeploy.yaml" : null
        AUTO_DEPLOY_ATTR_KEY   = local.autodeploy_rules ? null : var.autodeploy_downstream.attribute_key
        AUTO_DEPLOY_ATTR_VALUE = local.autodeploy_rules ? null : var.autodeploy_downstream.attribute_value
      })
      secrets     = local.massdriver_secrets
      event_types = ["deployment.completed"]
      attributes  = var.autodeploy_downstream.attributes
      condition   = ".data.metadata.deployment_action == \"PROVISION\""
    }

    bundle-scanner = {
      enabled = var.bundle_scanner.enabled
      image   = "${local.image}-scanner"
      host    = "bundle-scanner.${local.domain}"
      files   = {}
      state   = false
      env = merge(local.massdriver_env, {
        SCANNERS           = var.bundle_scanner.scanners
        SCAN_FAIL_SEVERITY = var.bundle_scanner.fail_severity
        SCAN_ATTRIBUTE_KEY = var.bundle_scanner.result_attribute_key
        SCAN_TIMEOUT       = var.bundle_scanner.timeout
      })
      secrets = merge(local.massdriver_secrets, {
        SLACK_WEBHOOK_URL = lookup(local.secrets, "BUNDLE_SCANNER_SLACK_WEBHOOK_URL", "")
      })
      event_types = ["repo.published"]
      attributes  = var.bundle_scanner.attributes
      condition   = ".data.entity.artifact_type == \"application/vnd.massdriver.bundle.v1+json\""
    }

    drift = {
      enabled = var.drift.enabled
      image   = local.image
      host    = "drift.${local.domain}"
      files   = { "drift.yaml" = var.drift.config }
      state   = true
      env = merge(local.massdriver_env, {
        DRIFT_CONFIG       = "${local.config_dir}/drift.yaml"
        STATE_DIR          = local.state_dir
        MASSDRIVER_APP_URL = var.massdriver.app_url
      })
      secrets = merge(local.massdriver_secrets, {
        SLACK_WEBHOOK_URL = lookup(local.secrets, "DRIFT_SLACK_WEBHOOK_URL", "")
        DRIFT_RUN_TOKEN   = lookup(local.secrets, "DRIFT_RUN_TOKEN", "")
      })
      event_types = ["deployment.completed", "deployment.failed", "deployment.aborted"]
      attributes  = var.drift.attributes
      condition   = ".data.metadata.deployment_action == \"PLAN\""
    }

    freeze-guard = {
      enabled = var.freeze_guard.enabled
      image   = local.image
      host    = "freeze-guard.${local.domain}"
      files   = { "windows.yaml" = var.freeze_guard.windows }
      state   = true
      env = merge(local.massdriver_env, {
        FREEZE_WINDOWS_FILE = "${local.config_dir}/windows.yaml"
        STATE_DIR           = local.state_dir
      })
      secrets = merge(local.massdriver_secrets, {
        FREEZE_ADMIN_TOKEN = lookup(local.secrets, "FREEZE_GUARD_ADMIN_TOKEN", "")
        SLACK_WEBHOOK_URL  = lookup(local.secrets, "FREEZE_GUARD_SLACK_WEBHOOK_URL", "")
      })
      event_types = ["group.created", "group.updated", "group.deleted"]
      attributes  = {}
      condition   = ""
    }

    github-deployments = {
      enabled = var.github_deployments.enabled
      image   = local.image
      host    = "github-deployments.${local.domain}"
      files   = {}
      state   = false
      env = {
        GITHUB_API_URL          = var.github_deployments.api_url
        MASSDRIVER_APP_URL      = var.massdriver.app_url
        GITHUB_REPO_ATTR_KEY    = var.github_deployments.repo_attribute_key
        GITHUB_REF_ATTR_KEY     = var.github_deployments.ref_attribute_key
        GITHUB_ENVIRONMENT_NAME = var.github_deployments.environment_name
      }
      secrets = {
        GITHUB_TOKEN = lookup(local.secrets, "GITHUB_DEPLOYMENTS_TOKEN", "")
      }
      event_types = ["deployment.started", "deployment.completed", "deployment.failed", "deployment.aborted"]
      attributes  = {}
      condition   = ".data.metadata.deployment_action != \"PLAN\" and (.data.attributes[${jsonencode(var.github_deployments.repo_attribute_key)}] // \"\") != \"\""
    }

    proposal-chatops = {
      enabled = var.proposal_chatops.enabled
      image   = local.image
      host    = "proposal-chatops.${local.domain}"
      files   = {}
      state   = false
      env = merge(local.massdriver_env, {
        SLACK_CHANNEL          = var.proposal_chatops.channel
        SLACK_CHANNEL_ATTR_KEY = var.proposal_chatops.channel_attribute_key
        SLACK_ALLOWED_USERS    = var.proposal_chatops.allowed_users
      })
      secrets = merge(local.massdriver_secrets, {
        SLACK_BOT_TOKEN      = lookup(local.secrets, "PROPOSAL_CHATOPS_SLACK_BOT_TOKEN", "")
        SLACK_SIGNING_SECRET = lookup(local.secrets, "PROPOSAL_CHATOPS_SLACK_SIGNING_SECRET", "")
      })
      event_types = ["deployment.proposed", "deployment.approved", "deployment.rejected"]
      attributes  = var.proposal_chatops.attributes
      condition   = ""
    }

    release-notes = {
      enabled = var.release_notes.enabled
      image   = local.image
      host    = "release-notes.${local.domain}"
      files   = {}
      state   = false
      env = merge(local.massdriver_env, {
        INCLUDE_DEVELOPMENT = tostring(var.release_notes.include_development)
      })
      secrets = merge(local.massdriver_secrets, {
        SLACK_WEBHOOK_URL = lookup(local.secrets, "RELEASE_NOTES_SLACK_WEBHOOK_URL", "")
        TEAMS_WEBHOOK_URL = lookup(local.secrets, "RELEASE_NOTES_TEAMS_WEBHOOK_URL", "")
      })
      event_types = ["repo.published"]
      attributes  = { "md-repo-artifact-type" = "bundle" }
      condition   = ""
    }

    rollback-on-failure = {
      enabled = var.rollback_on_failure.enabled
      image   = local.image
      host    = "rollback-on-failure.${local.domain}"
      files   = {}
      state   = false
      env = merge(local.massdriver_env, {
        AUTO_ROLLBACK_ATTR_KEY   = var.rollback_on_failure.attribute_key
        AUTO_ROLLBACK_ATTR_VALUE = var.rollback_on_failure.attribute_value
        MASSDRIVER_APP_URL       = var.massdriver.app_url
      })
      secrets = merge(local.massdriver_secrets, {
        SLACK_WEBHOOK_URL = lookup(local.secrets, "ROLLBACK_ON_FAILURE_SLACK_WEBHOOK_URL", "")
      })
      event_types = ["deployment.failed"]
      attributes  = var.rollback_on_failure.attributes
      condition   = ".data.metadata.deployment_action == \"PROVISION\" and .data.attributes[${jsonencode(var.rollback_on_failure.attribute_key)}] == ${jsonencode(var.rollback_on_failure.attribute_value)}"
    }

    router = {
      enabled = var.router.enabled
      image   = local.router_uses_exec ? "${local.image}-scripts" : local.image
      host    = "router.${local.domain}"
      files   = { "config.yaml" = var.router.config }
      state   = false
      env = merge(local.massdriver_env, {
        ROUTER_CONFIG = "${local.config_dir}/config.yaml"
      })
      secrets     = merge(local.router_env, local.massdriver_secrets)
      event_types = local.router_event_types
      # Routes filter on attributes and jq themselves, and an attribute filter would drop events without attributes.
      attributes = {}
      condition  = ""
    }

    sandbox-on-join = {
      enabled = var.sandbox_on_join.enabled
      image   = local.image
      host    = "sandbox-on-join.${local.domain}"
      files   = { "sandboxes.yaml" = var.sandbox_on_join.config }
      state   = false
      env = merge(local.massdriver_env, {
        SANDBOX_CONFIG = "${local.config_dir}/sandboxes.yaml"
      })
      secrets     = local.massdriver_secrets
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
      enabled = var.statuspage.enabled
      image   = local.image
      host    = local.statuspage_host
      files   = { "statuspage.yaml" = var.statuspage.config }
      state   = true
      env = merge(local.massdriver_env, {
        STATUSPAGE_CONFIG = "${local.config_dir}/statuspage.yaml"
        STATE_DIR         = local.state_dir
      })
      secrets = local.massdriver_secrets
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

  enabled  = { for name, a in local.automations : name => a if a.enabled }
  stateful = { for name, a in local.enabled : name => a if a.state }

  # Unset settings are left out, so each automation uses its own default.
  settings = {
    for name, a in local.enabled : name => { for k, v in a.env : k => tostring(v) if v != null && v != "" }
  }

  secret_env = {
    for name, a in local.enabled : name => merge(
      { for k, v in a.secrets : k => tostring(v) if v != null && v != "" },
      { MASSDRIVER_SIGNING_SECRET = massdriver_event_destination.main[name].signing_secret }
    )
  }

  # Written to the task's volumes by the init container, because the image has no shell and Fargate has no
  # equivalent of a ConfigMap.
  files = {
    for name, a in local.enabled : name => concat(
      [for f, content in a.files : { path = "${local.config_dir}/${f}", mode = "0644", content = content }],
      name == "router" ? [
        for f, content in var.router.scripts : { path = "${local.scripts_dir}/${f}", mode = "0755", content = content }
      ] : [],
    )
  }

  # ECS, IAM, and target group names have different length limits. The longest automation name is 21 characters.
  names      = { for name, _ in local.automations : name => "${trimsuffix(substr(local.name_prefix, 0, 41), "-")}-${name}" }
  role_names = { for name, _ in local.automations : name => "${trimsuffix(substr(local.name_prefix, 0, 36), "-")}-${name}" }
  target_group_names = {
    for name, _ in local.automations : name => "${substr(sha1("${local.name_prefix}-${name}"), 0, 10)}-${name}"
  }

  zone_id     = try(var.ecs_cluster.dns.zone_id, "")
  zone_domain = try(var.ecs_cluster.dns.base_domain, "")
  dns_hosts = {
    for name, a in local.enabled : name => a.host
    if var.ingress.dns_records && local.zone_id != "" && endswith(a.host, ".${local.zone_domain}")
  }
}
