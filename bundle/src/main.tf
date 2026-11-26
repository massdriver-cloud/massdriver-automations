locals {
  # Bundle secrets are not OpenTofu variables. The provisioner writes them to this file.
  secrets = try(jsondecode(file("/massdriver/secrets.json")), {})

  namespace   = coalesce(try(var.kubernetes_cluster.namespace, null), "default")
  name_prefix = var.md_metadata.name_prefix
  config_dir  = "/etc/massdriver-automations"
  state_dir   = "/var/lib/statuspage"

  image = "${var.image.repository}:${var.image.tag}"

  massdriver_env = {
    MASSDRIVER_ORGANIZATION_ID = var.service_account.organization_id
    MASSDRIVER_URL             = var.service_account.url
    MASSDRIVER_API_KEY         = var.service_account.api_key
  }

  # The secret may arrive as a JSON string or as an object.
  router_env = try(jsondecode(local.secrets.ROUTER_ENV), lookup(local.secrets, "ROUTER_ENV", {}))

  # One Slack app serves every automation that posts.
  slack_env = {
    SLACK_BOT_TOKEN   = lookup(local.secrets, "SLACK_BOT_TOKEN", "")
    SLACK_WEBHOOK_URL = lookup(local.secrets, "SLACK_WEBHOOK_URL", "")
  }

  # Each automation reads its configuration at CONFIG_FILE, whose default is this path.
  config_file = "config.yaml"

  # Each form holds the settings of an automation in the shape of its config file. Attribute values with commas
  # become lists, which match any of their values.
  audit_archiver_form = try(var.audit_archiver.config, null)
  autodeploy_form     = try(var.autodeploy_downstream.config, null)
  drift_form          = try(var.drift.config, null)
  freeze_form         = try(var.freeze_guard.config, null)

  configs = {
    audit-archiver = local.audit_archiver_form == null ? null : merge(
      {
        for sink in ["disk", "splunk", "datadog"] : sink => {
          for k, v in local.audit_archiver_form[sink] : k => v if k != "enabled" && v != null
        } if try(local.audit_archiver_form[sink].enabled, false)
      },
      try(local.audit_archiver_form.s3.enabled, false) ? {
        s3 = merge(
          {
            for k, v in local.audit_archiver_form.s3 : k => v
            if v != null && !contains(["enabled", "object_lock_days", "object_lock_mode"], k)
          },
          coalesce(local.audit_archiver_form.s3.object_lock_days, 0) > 0 ? {
            object_lock = {
              mode = coalesce(local.audit_archiver_form.s3.object_lock_mode, "COMPLIANCE")
              days = local.audit_archiver_form.s3.object_lock_days
            }
          } : null,
        )
      } : null,
    )
    autodeploy-downstream = local.autodeploy_form == null ? null : {
      rules = [
        for r in coalesce(local.autodeploy_form.rules, []) : {
          name       = r.name
          action     = r.action
          downstream = { for k, v in r.downstream : k => [for x in split(",", v) : trimspace(x)] }
          upstream   = { for k, v in coalesce(r.upstream, {}) : k => [for x in split(",", v) : trimspace(x)] }
        }
      ]
    }
    bundle-scanner = try(var.bundle_scanner.config, null)
    drift = local.drift_form == null ? null : merge(local.drift_form, {
      checks = [
        for c in coalesce(local.drift_form.checks, []) : merge(c, {
          match = { for k, v in c.match : k => [for x in split(",", v) : trimspace(x)] }
        })
      ]
    })
    freeze-guard = local.freeze_form == null ? null : merge(local.freeze_form, {
      windows = [
        for w in coalesce(local.freeze_form.windows, []) : merge(w, {
          match = { for k, v in coalesce(w.match, {}) : k => [for x in split(",", v) : trimspace(x)] }
        })
      ]
    })
    github-deployments  = try(var.github_deployments.config, null)
    proposal-chatops    = try(var.proposal_chatops.config, null)
    release-notes       = try(var.release_notes.config, null)
    rollback-on-failure = try(var.rollback_on_failure.config, null)
    # router's routes are free-form, so its form holds the YAML itself.
    router          = try(yamldecode(var.router.config), {})
    sandbox-on-join = try(var.sandbox_on_join.config, null)
    statuspage      = try(var.statuspage.config, null)
  }

  config_files = {
    for name, c in local.configs : name => name == "router" ? try(tostring(var.router.config), "") : c == null ? "" : yamlencode(c)
  }
  statuspage_config = local.configs.statuspage
  sandbox_config    = local.configs["sandbox-on-join"]
  router_config     = local.configs.router

  # The same defaults as the automations, so the destination conditions match what they act on.
  github_repo_attribute    = try(coalesce(local.configs["github-deployments"].github.repo_attribute), "github_repo")
  rollback_attribute_key   = try(coalesce(local.configs["rollback-on-failure"].attribute.key), "autorollback")
  rollback_attribute_value = try(tostring(coalesce(local.configs["rollback-on-failure"].attribute.value)), "true")

  router_routes      = try(coalesce(local.router_config.routes), [])
  router_event_types = distinct(flatten([for r in local.router_routes : try(coalesce(r.events), [])]))
  # Commands need a shell, jq, curl, or python3, which only the -scripts image has.
  router_scripts = coalesce(try(var.router.scripts, null), {})
  router_uses_exec = length(local.router_scripts) > 0 || anytrue(flatten([
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
  # A rule names its group by name or by ID.
  sandbox_group_list = join(", ", [for g in local.sandbox_groups : jsonencode(g)])
  sandbox_group_test = (length(local.sandbox_groups) > 0
    ? "((.data.entity.group_name | IN(${local.sandbox_group_list})) or (.data.entity.group_id | IN(${local.sandbox_group_list})))"
  : "false")
  statuspage_environment_test = (length(local.statuspage_environments) > 0
    ? "($env | IN(${join(", ", [for e in local.statuspage_environments : jsonencode(e)])}))"
  : "false")

  # statuspage writes the static site to output_dir, when its config sets one.
  statuspage_output_dir = try(trimsuffix(coalesce(local.statuspage_config.output_dir), "/"), "")
  statuspage_site       = var.statuspage.enabled && local.statuspage_output_dir != ""

  # statuspage makes a slug from the title of a page without one.
  statuspage_pages = [
    for p in try(local.statuspage_config.pages, []) : {
      title = p.title
      slug  = coalesce(try(p.slug, null), trim(replace(lower(p.title), "/[^a-z0-9]+/", "-"), "-"))
    }
  ]

  statuspage_host = coalesce(var.statuspage.host, "statuspage.${var.ingress.domain}")

  ingress_class   = try(coalesce(var.ingress.class_name), null)
  cluster_issuer  = try(coalesce(var.ingress.cluster_issuer), "")
  tls_secret_name = try(coalesce(var.ingress.tls_secret_name), "")

  automations = {
    audit-archiver = {
      enabled    = var.audit_archiver.enabled
      image      = local.image
      host       = "audit-archiver.${var.ingress.domain}"
      files      = { (local.config_file) = local.config_files["audit-archiver"] }
      state_size = 0
      env = {
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
      host        = "autodeploy-downstream.${var.ingress.domain}"
      files       = { (local.config_file) = local.config_files["autodeploy-downstream"] }
      state_size  = 0
      env         = local.massdriver_env
      event_types = ["deployment.completed"]
      attributes  = coalesce(var.autodeploy_downstream.attributes, {})
      # The rules' upstream attributes are left to the automation, which checks them on each event.
      condition = ".data.metadata.deployment_action == \"PROVISION\""
    }

    bundle-scanner = {
      enabled     = var.bundle_scanner.enabled
      image       = "${local.image}-scanner"
      host        = "bundle-scanner.${var.ingress.domain}"
      files       = { (local.config_file) = local.config_files["bundle-scanner"] }
      state_size  = 0
      env         = merge(local.massdriver_env, local.slack_env)
      event_types = ["repo.published"]
      attributes  = coalesce(var.bundle_scanner.attributes, {})
      condition   = ".data.entity.artifact_type == \"application/vnd.massdriver.bundle.v1+json\""
    }

    drift = {
      enabled    = var.drift.enabled
      image      = local.image
      host       = "drift.${var.ingress.domain}"
      files      = { (local.config_file) = local.config_files["drift"] }
      state_size = coalesce(var.drift.storage_size_gi, 1)
      env = merge(local.massdriver_env, local.slack_env, {
        STATE_DIR       = local.state_dir
        DRIFT_RUN_TOKEN = lookup(local.secrets, "DRIFT_RUN_TOKEN", "")
      })
      event_types = ["deployment.completed", "deployment.failed", "deployment.aborted"]
      attributes  = coalesce(var.drift.attributes, {})
      condition   = ".data.metadata.deployment_action == \"PLAN\""
    }

    freeze-guard = {
      enabled    = var.freeze_guard.enabled
      image      = local.image
      host       = "freeze-guard.${var.ingress.domain}"
      files      = { (local.config_file) = local.config_files["freeze-guard"] }
      state_size = coalesce(var.freeze_guard.storage_size_gi, 1)
      env = merge(local.massdriver_env, local.slack_env, {
        STATE_DIR          = local.state_dir
        FREEZE_ADMIN_TOKEN = lookup(local.secrets, "FREEZE_GUARD_ADMIN_TOKEN", "")
      })
      # Group events have no attributes, and freeze-guard reconciles every group on each one.
      event_types = ["group.created", "group.updated", "group.deleted"]
      attributes  = {}
      condition   = ""
    }

    github-deployments = {
      enabled    = var.github_deployments.enabled
      image      = local.image
      host       = "github-deployments.${var.ingress.domain}"
      files      = { (local.config_file) = local.config_files["github-deployments"] }
      state_size = 0
      env = {
        GITHUB_TOKEN = lookup(local.secrets, "GITHUB_DEPLOYMENTS_TOKEN", "")
      }
      event_types = ["deployment.started", "deployment.completed", "deployment.failed", "deployment.aborted"]
      attributes  = {}
      condition   = ".data.metadata.deployment_action != \"PLAN\" and (.data.attributes[${jsonencode(local.github_repo_attribute)}] // \"\") != \"\""
    }

    proposal-chatops = {
      enabled    = var.proposal_chatops.enabled
      image      = local.image
      host       = "proposal-chatops.${var.ingress.domain}"
      files      = { (local.config_file) = local.config_files["proposal-chatops"] }
      state_size = 0
      env = merge(local.massdriver_env, {
        SLACK_BOT_TOKEN      = lookup(local.secrets, "SLACK_BOT_TOKEN", "")
        SLACK_SIGNING_SECRET = lookup(local.secrets, "SLACK_SIGNING_SECRET", "")
      })
      event_types = ["deployment.proposed", "deployment.approved", "deployment.rejected"]
      attributes  = coalesce(var.proposal_chatops.attributes, {})
      condition   = ""
    }

    release-notes = {
      enabled    = var.release_notes.enabled
      image      = local.image
      host       = "release-notes.${var.ingress.domain}"
      files      = { (local.config_file) = local.config_files["release-notes"] }
      state_size = 0
      env = merge(local.massdriver_env, local.slack_env, {
        TEAMS_WEBHOOK_URL = lookup(local.secrets, "RELEASE_NOTES_TEAMS_WEBHOOK_URL", "")
      })
      event_types = ["repo.published"]
      attributes  = { "md-repo-artifact-type" = "bundle" }
      condition   = ""
    }

    rollback-on-failure = {
      enabled     = var.rollback_on_failure.enabled
      image       = local.image
      host        = "rollback-on-failure.${var.ingress.domain}"
      files       = { (local.config_file) = local.config_files["rollback-on-failure"] }
      state_size  = 0
      env         = merge(local.massdriver_env, local.slack_env)
      event_types = ["deployment.failed", "deployment.completed"]
      attributes  = coalesce(var.rollback_on_failure.attributes, {})
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
      host        = "router.${var.ingress.domain}"
      files       = { (local.config_file) = local.config_files["router"] }
      state_size  = 0
      env         = merge(local.router_env, local.massdriver_env)
      event_types = local.router_event_types
      # Routes filter on attributes and jq themselves, and an attribute filter would drop events without attributes.
      attributes = {}
      condition  = ""
    }

    sandbox-on-join = {
      enabled     = var.sandbox_on_join.enabled
      image       = local.image
      host        = "sandbox-on-join.${var.ingress.domain}"
      files       = { (local.config_file) = local.config_files["sandbox-on-join"] }
      state_size  = 0
      env         = local.massdriver_env
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
      files      = { (local.config_file) = local.config_files["statuspage"] }
      state_size = coalesce(var.statuspage.storage_size_gi, 1)
      env = merge(local.massdriver_env, {
        STATE_DIR = local.state_dir
      })
      event_types = [
        "alarm.*", "alarm_state.created", "deployment.started", "deployment.completed", "deployment.failed",
        "deployment.aborted", "instance.*",
      ]
      attributes = coalesce(var.statuspage.attributes, {})
      condition  = <<-JQ
        (.data.attributes["md-project"] + "-" + .data.attributes["md-environment"]) as $env
        | ${local.statuspage_environment_test}
          and (.data.metadata.deployment_action // "PROVISION") != "PLAN"
      JQ
    }
  }

  enabled = { for name, a in local.automations : name => a if a.enabled }

  otel_env = {
    OTEL_EXPORTER_OTLP_ENDPOINT = try(var.otel.endpoint, null)
    OTEL_EXPORTER_OTLP_HEADERS  = lookup(local.secrets, "OTEL_EXPORTER_OTLP_HEADERS", "")
  }

  # Unset settings are left out, so each automation uses its own default.
  settings = {
    for name, a in local.enabled : name => {
      for k, v in merge(a.env, local.otel_env) : k => tostring(v) if v != null && v != ""
    }
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
