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

  # One Slack app serves every automation that posts.
  slack_secrets = {
    SLACK_BOT_TOKEN   = lookup(local.secrets, "SLACK_BOT_TOKEN", "")
    SLACK_WEBHOOK_URL = lookup(local.secrets, "SLACK_WEBHOOK_URL", "")
  }

  # Each automation reads its configuration at CONFIG_FILE, whose default is this path.
  config_file = "config.yaml"

  configs = {
    audit-archiver      = try(yamldecode(var.audit_archiver.config), {})
    github-deployments  = try(yamldecode(var.github_deployments.config), {})
    proposal-chatops    = try(yamldecode(var.proposal_chatops.config), {})
    rollback-on-failure = try(yamldecode(var.rollback_on_failure.config), {})
    router              = try(yamldecode(var.router.config), {})
    sandbox-on-join     = try(yamldecode(var.sandbox_on_join.config), {})
    statuspage          = try(yamldecode(var.statuspage.config), {})
  }
  statuspage_config = local.configs.statuspage
  sandbox_config    = local.configs["sandbox-on-join"]
  router_config     = local.configs.router

  # The same defaults as the automations, so the destination conditions match what they act on.
  github_repo_attribute    = try(coalesce(local.configs["github-deployments"].github.repo_attribute), "github_repo")
  rollback_attribute_key   = try(coalesce(local.configs["rollback-on-failure"].attribute.key), "autorollback")
  rollback_attribute_value = try(tostring(coalesce(local.configs["rollback-on-failure"].attribute.value)), "true")

  audit_archiver_s3 = try(local.configs["audit-archiver"].s3, null)

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
    try(coalesce(local.audit_archiver_s3.bucket), "") != ""
    && try(coalesce(local.audit_archiver_s3.endpoint), "") == ""
    && !local.audit_archiver_static_keys
  )
  audit_archiver_s3_bucket      = try(coalesce(local.audit_archiver_s3.bucket), "")
  audit_archiver_s3_prefix      = try(coalesce(trim(local.audit_archiver_s3.prefix, "/")), "events")
  audit_archiver_s3_object_lock = try(local.audit_archiver_s3.object_lock, null) != null

  automations = {
    audit-archiver = {
      enabled = var.audit_archiver.enabled
      image   = local.image
      host    = "audit-archiver.${local.domain}"
      files   = { (local.config_file) = var.audit_archiver.config }
      state   = false
      env     = {}
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
      enabled     = var.autodeploy_downstream.enabled
      image       = local.image
      host        = "autodeploy-downstream.${local.domain}"
      files       = { (local.config_file) = var.autodeploy_downstream.config }
      state       = false
      env         = local.massdriver_env
      secrets     = local.massdriver_secrets
      event_types = ["deployment.completed"]
      attributes  = var.autodeploy_downstream.attributes
      # The rules' upstream attributes are left to the automation, which checks them on each event.
      condition = ".data.metadata.deployment_action == \"PROVISION\""
    }

    bundle-scanner = {
      enabled     = var.bundle_scanner.enabled
      image       = "${local.image}-scanner"
      host        = "bundle-scanner.${local.domain}"
      files       = { (local.config_file) = var.bundle_scanner.config }
      state       = false
      env         = local.massdriver_env
      secrets     = merge(local.massdriver_secrets, local.slack_secrets)
      event_types = ["repo.published"]
      attributes  = var.bundle_scanner.attributes
      condition   = ".data.entity.artifact_type == \"application/vnd.massdriver.bundle.v1+json\""
    }

    drift = {
      enabled = var.drift.enabled
      image   = local.image
      host    = "drift.${local.domain}"
      files   = { (local.config_file) = var.drift.config }
      state   = true
      env = merge(local.massdriver_env, {
        STATE_DIR = local.state_dir
      })
      secrets = merge(local.massdriver_secrets, local.slack_secrets, {
        DRIFT_RUN_TOKEN = lookup(local.secrets, "DRIFT_RUN_TOKEN", "")
      })
      event_types = ["deployment.completed", "deployment.failed", "deployment.aborted"]
      attributes  = var.drift.attributes
      condition   = ".data.metadata.deployment_action == \"PLAN\""
    }

    freeze-guard = {
      enabled = var.freeze_guard.enabled
      image   = local.image
      host    = "freeze-guard.${local.domain}"
      files   = { (local.config_file) = var.freeze_guard.config }
      state   = true
      env = merge(local.massdriver_env, {
        STATE_DIR = local.state_dir
      })
      secrets = merge(local.massdriver_secrets, local.slack_secrets, {
        FREEZE_ADMIN_TOKEN = lookup(local.secrets, "FREEZE_GUARD_ADMIN_TOKEN", "")
      })
      # Group events have no attributes, and freeze-guard reconciles every group on each one.
      event_types = ["group.created", "group.updated", "group.deleted"]
      attributes  = {}
      condition   = ""
    }

    github-deployments = {
      enabled = var.github_deployments.enabled
      image   = local.image
      host    = "github-deployments.${local.domain}"
      files   = { (local.config_file) = var.github_deployments.config }
      state   = false
      env     = {}
      secrets = {
        GITHUB_TOKEN = lookup(local.secrets, "GITHUB_DEPLOYMENTS_TOKEN", "")
      }
      event_types = ["deployment.started", "deployment.completed", "deployment.failed", "deployment.aborted"]
      attributes  = {}
      condition   = ".data.metadata.deployment_action != \"PLAN\" and (.data.attributes[${jsonencode(local.github_repo_attribute)}] // \"\") != \"\""
    }

    proposal-chatops = {
      enabled = var.proposal_chatops.enabled
      image   = local.image
      host    = "proposal-chatops.${local.domain}"
      files   = { (local.config_file) = var.proposal_chatops.config }
      state   = false
      env     = local.massdriver_env
      secrets = merge(local.massdriver_secrets, {
        SLACK_BOT_TOKEN      = lookup(local.secrets, "SLACK_BOT_TOKEN", "")
        SLACK_SIGNING_SECRET = lookup(local.secrets, "SLACK_SIGNING_SECRET", "")
      })
      event_types = ["deployment.proposed", "deployment.approved", "deployment.rejected"]
      attributes  = var.proposal_chatops.attributes
      condition   = ""
    }

    release-notes = {
      enabled = var.release_notes.enabled
      image   = local.image
      host    = "release-notes.${local.domain}"
      files   = { (local.config_file) = var.release_notes.config }
      state   = false
      env     = local.massdriver_env
      secrets = merge(local.massdriver_secrets, local.slack_secrets, {
        TEAMS_WEBHOOK_URL = lookup(local.secrets, "RELEASE_NOTES_TEAMS_WEBHOOK_URL", "")
      })
      event_types = ["repo.published"]
      attributes  = { "md-repo-artifact-type" = "bundle" }
      condition   = ""
    }

    rollback-on-failure = {
      enabled     = var.rollback_on_failure.enabled
      image       = local.image
      host        = "rollback-on-failure.${local.domain}"
      files       = { (local.config_file) = var.rollback_on_failure.config }
      state       = false
      env         = local.massdriver_env
      secrets     = merge(local.massdriver_secrets, local.slack_secrets)
      event_types = ["deployment.failed", "deployment.completed"]
      attributes  = var.rollback_on_failure.attributes
      # A completed deployment matters only when it is a rollback, whose result goes in the notice's thread.
      condition = join(" ", [
        ".data.metadata.deployment_action == \"PROVISION\"",
        "and .data.attributes[${jsonencode(local.rollback_attribute_key)}] == ${jsonencode(local.rollback_attribute_value)}",
        "and (.type == \"deployment.failed\" or ((.data.entity.message // \"\") | startswith(\"Rollback to deployment \")))",
      ])
    }

    router = {
      enabled     = var.router.enabled
      image       = local.router_uses_exec ? "${local.image}-scripts" : local.image
      host        = "router.${local.domain}"
      files       = { (local.config_file) = var.router.config }
      state       = false
      env         = local.massdriver_env
      secrets     = merge(local.router_env, local.massdriver_secrets)
      event_types = local.router_event_types
      # Routes filter on attributes and jq themselves, and an attribute filter would drop events without attributes.
      attributes = {}
      condition  = ""
    }

    sandbox-on-join = {
      enabled     = var.sandbox_on_join.enabled
      image       = local.image
      host        = "sandbox-on-join.${local.domain}"
      files       = { (local.config_file) = var.sandbox_on_join.config }
      state       = false
      env         = local.massdriver_env
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
      files   = { (local.config_file) = var.statuspage.config }
      state   = true
      env = merge(local.massdriver_env, {
        STATE_DIR = local.state_dir
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
    for name, a in local.enabled : name => {
      for k, v in merge(a.env, { OTEL_EXPORTER_OTLP_ENDPOINT = try(var.otel.endpoint, null) }) :
      k => tostring(v) if v != null && v != ""
    }
  }

  otel_secrets = {
    OTEL_EXPORTER_OTLP_HEADERS = lookup(local.secrets, "OTEL_EXPORTER_OTLP_HEADERS", "")
  }

  secret_env = {
    for name, a in local.enabled : name => merge(
      { for k, v in merge(a.secrets, local.otel_secrets) : k => tostring(v) if v != null && v != "" },
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
