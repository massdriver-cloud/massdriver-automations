"""Writes the automation settings of both bundles' massdriver.yaml: the params between audit_archiver and otel,
and their ui schema. Run it from the repository root after changing an automation's config file format.

    python3 scripts/bundle-params.py
"""
import os

import yaml

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
README = "https://github.com/massdriver-cloud/massdriver-automations/tree/main/automations/{}"


def s(title, desc=None, **kw):
    d = {"type": "string", "title": title}
    if desc:
        d["description"] = desc
    d.update(kw)
    return d


def b(title, desc=None, default=False):
    d = {"type": "boolean", "title": title}
    if desc:
        d["description"] = desc
    d["default"] = default
    return d


def i(title, desc=None, **kw):
    d = {"type": "integer", "title": title}
    if desc:
        d["description"] = desc
    d.update(kw)
    return d


def obj(title, props, desc=None, required=None, **kw):
    d = {"type": "object", "title": title}
    if desc:
        d["description"] = desc
    if required:
        d["required"] = required
    d["properties"] = props
    d.update(kw)
    return d


def arr(title, items, desc=None, **kw):
    d = {"type": "array", "title": title}
    if desc:
        d["description"] = desc
    d["items"] = items
    d.update(kw)
    return d


def choice(title, options, desc=None, default=None):
    d = {"type": "string", "title": title}
    if desc:
        d["description"] = desc
    d["oneOf"] = [{"const": v, "title": t} for v, t in options]
    if default is not None:
        d["default"] = default
    return d


def string_map(title, desc):
    return {"type": "object", "title": title, "description": desc, "default": {},
            "additionalProperties": {"type": "string"}}


def values_map(title, desc):
    return string_map(title, desc + " Separate values with commas to match any of them, such as `api, web`.")


TIMEZONE = dict(examples=["America/New_York"], pattern=r"^[A-Za-z_]+(/[A-Za-z0-9_+-]+)*$",
                message={"pattern": "Must be an IANA time zone, such as America/New_York or UTC."})
DURATION = dict(pattern=r"^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$",
                message={"pattern": "Must be a duration, such as 90s, 10m, or 1h30m."})
HTTPS = dict(pattern=r"^https?://[^\s]+$", message={"pattern": "Must be a URL."})
CHANNEL = dict(pattern=r"^([#@]?[a-z0-9._-]+|[CG][A-Z0-9]+)$",
               message={"pattern": "Must be a channel name, such as #infra, or a channel ID."})


def slack(desc):
    return obj("Slack", {"channel": s("Channel", desc, **CHANNEL)})


def massdriver_ui():
    return obj("Massdriver UI", {"app_url": s(
        "App URL", "The Massdriver UI, for the links in messages. Empty uses https://app.massdriver.cloud.", **HTTPS)})


ATTRIBUTES = string_map(
    "Event attributes",
    "Attribute values that an event's entity must have, such as `md-environment` = `prod`. Empty sends events of "
    "any attributes.")


def storage(title, desc):
    return i(title, desc, default=1, minimum=1, maximum=10, **{"$md.immutable": True})


GROUPS = {"type": "string", "$md.list": {"resource": "group"}}

DEPLOY_ACTIONS = [("instance:deploy", "Deploy instances"), ("instance:decommission", "Decommission instances"),
                  ("environment:deploy", "Deploy environments"),
                  ("environment:decommission", "Decommission environments")]


TAG = {"ui:field": "tagInput"}


def automations(k8s):
    out = {}

    # ---------------------------------------------------------------- audit-archiver
    def sink(title, desc, props, required):
        return {
            "type": "object", "title": title, "description": desc,
            "required": ["enabled"],
            "properties": {"enabled": b(f"Archive to {title}")},
            "dependencies": {"enabled": {"oneOf": [
                {"properties": {"enabled": {"const": False}}},
                {"properties": {"enabled": {"const": True}, **props}, "required": required},
            ]}},
        }

    out["audit-archiver"] = dict(
        title="Audit archiver",
        description="Archives every event, unchanged, to disk, S3, Splunk, or Datadog. Every event goes to every "
                    "sink that is on.",
        config=obj("Sinks", {
            "s3": sink("S3", "S3 or an S3-compatible store. Credentials come from the AWS secrets"
                       + ("." if k8s else ", or the task role when they are empty."), {
                "bucket": s("Bucket", examples=["acme-massdriver-audit"]),
                "region": s("Region", "The region of the bucket. Use `auto` for Cloudflare R2.",
                            examples=["us-east-1"]),
                "prefix": s("Key prefix", "The first part of each key.", default="events"),
                "endpoint": s("Endpoint", "The URL of an S3-compatible store, such as MinIO or Cloudflare R2. "
                              "Empty uses AWS.", **HTTPS),
                "object_lock_days": i("Object Lock retention (days)",
                                      "Retains each object for this many days. The bucket must have Object Lock "
                                      "turned on. 0 sets no retention.", default=0, minimum=0),
                "object_lock_mode": choice("Object Lock mode", [("COMPLIANCE", "Compliance"),
                                                                ("GOVERNANCE", "Governance")],
                                           default="COMPLIANCE"),
            }, ["bucket", "region"]),
            "splunk": sink("Splunk", "A Splunk HTTP Event Collector. The `AUDIT_ARCHIVER_SPLUNK_HEC_TOKEN` secret "
                           "holds the token.", {
                "url": s("Collector URL", examples=["https://splunk.example.com:8088"], **HTTPS),
                "index": s("Index", "Empty uses the default index of the token."),
                "source": s("Source", default="massdriver"),
                "sourcetype": s("Source type", default="massdriver:cloudevent"),
            }, ["url"]),
            "datadog": sink("Datadog", "The Datadog Logs intake. The `AUDIT_ARCHIVER_DD_API_KEY` secret holds the "
                            "API key.", {
                "site": s("Site", "Your Datadog site, such as datadoghq.eu or us5.datadoghq.com.",
                          default="datadoghq.com"),
                "service": s("Service", default="massdriver"),
                "tags": arr("Tags", s("Tag", examples=["env:prod"]), default=[]),
            }, []),
            "disk": sink("Disk", "A directory in the container. Files are lost when the container restarts, so use "
                         "it only to test.", {
                "dir": s("Directory", default="/var/lib/audit-archiver", pattern="^/.+"),
            }, ["dir"]),
        }),
        extra={},
        ui={"ui:order": ["s3", "splunk", "datadog", "disk"],
            "s3": {"ui:order": ["enabled", "*"]}, "splunk": {"ui:order": ["enabled", "*"]},
            "datadog": {"ui:order": ["enabled", "*"], "tags": TAG}, "disk": {"ui:order": ["enabled", "*"]}},
    )

    # ---------------------------------------------------------------- autodeploy-downstream
    out["autodeploy-downstream"] = dict(
        title="Autodeploy downstream",
        description="Deploys the dependents of an instance after it deploys. Each rule selects dependents by their "
                    "attributes. The first rule that matches a dependent applies.",
        config=obj("Rules", {
            "rules": arr("Rules", obj("Rule", {
                "name": s("Name", "Unique. The logs and the deployment message name the rule.",
                          pattern="^[a-z0-9][a-z0-9-]*$",
                          message={"pattern": "Must be lowercase letters, numbers, and hyphens."}),
                "downstream": values_map("Dependent attributes",
                                         "Attributes that a dependent must have to deploy, such as `autodeploy` = "
                                         "`true`."),
                "upstream": values_map("Upstream attributes",
                                       "Attributes that the instance that deployed must have. Empty matches every "
                                       "instance."),
                "action": choice("Action", [("deploy", "Deploy now"),
                                            ("propose", "Propose a deployment for a person to approve")],
                                 default="deploy"),
            }, required=["name", "downstream", "action"]), minItems=1, default=[
                {"name": "autodeploy", "downstream": {"autodeploy": "true"}, "upstream": {}, "action": "deploy"}]),
        }),
        extra={"attributes": ATTRIBUTES},
        ui={"rules": {"ui:options": {"itemTitle": "{{name}}"},
                      "items": {"ui:order": ["name", "action", "downstream", "upstream"],
                                "action": {"ui:widget": "radio"}}}},
    )

    # ---------------------------------------------------------------- bundle-scanner
    out["bundle-scanner"] = dict(
        title="Bundle scanner",
        description="Scans each new bundle version with Trivy and Checkov, and posts the results.",
        config=obj("Scan", {
            "scanners": arr("Scanners", {"type": "string", "enum": ["trivy", "checkov"]},
                            uniqueItems=True, minItems=1, default=["trivy", "checkov"]),
            "fail_severity": choice("Fail at", [("CRITICAL", "Critical"), ("HIGH", "High or higher"),
                                                ("MEDIUM", "Medium or higher"), ("LOW", "Low or higher"),
                                                ("UNKNOWN", "Any finding")],
                                    "A version fails when a finding has this severity or higher.", default="HIGH"),
            "result_attribute": s("Result attribute",
                                  "A repo custom attribute that records the result as `passed` or `failed`. Empty "
                                  "sets no attribute.", examples=["iac_scan"]),
            "timeout": s("Timeout", "The longest time for one bundle version.", default="10m", **DURATION),
            "slack": slack("The channel for results. Empty posts nothing."),
        }),
        extra={"attributes": ATTRIBUTES},
        ui={"ui:order": ["scanners", "fail_severity", "slack", "result_attribute", "timeout"],
            "scanners": {"ui:widget": "checkboxes", "ui:options": {"inline": True}}},
    )

    # ---------------------------------------------------------------- cost-explorer
    extra = {"host": s("Host", "The host of the dashboard, such as `costs.acme.com`. Empty uses "
                       "`cost-explorer.<domain>`.")}
    if k8s:
        extra["storage_size_gi"] = storage("History storage (GiB)", "The volume for the daily cost history.")
    out["cost-explorer"] = dict(
        title="Cost explorer",
        description="Serves a dashboard of the cloud costs of every instance. Group and filter by any instance "
                    "attribute, and see the daily trend. Set the `COST_EXPLORER_PASSWORD` secret, or make it "
                    "public.",
        config=obj("Dashboard", {
            "title": s("Title", default="Cloud costs"),
            "dimensions": arr("Dimensions", s("Attribute", examples=["team"]),
                              "Instance attributes to group and filter by, in this order. `bundle` is the bundle "
                              "name. Only these attributes reach the browser.", uniqueItems=True, minItems=1,
                              default=["md-project", "md-environment", "md-component", "bundle"]),
            "refresh": s("Refresh", "How often to read the costs. A completed provision or decommission also "
                         "reads them.", default="1h", **DURATION),
            "history_days": i("History (days)", "Days of daily cost history to keep.", default=90, minimum=1,
                              maximum=731),
            "public": b("Public", "Serves the dashboard without a password. Costs can reveal what you run."),
            "username": s("Username", "The basic auth user. The `COST_EXPLORER_PASSWORD` secret is the "
                          "password.", default="massdriver"),
            "massdriver": massdriver_ui(),
        }),
        extra=extra,
        ui={"ui:order": ["title", "dimensions", "refresh", "history_days", "public", "username", "massdriver"],
            "dimensions": TAG},
    )

    # ---------------------------------------------------------------- drift
    check = obj("Check", {
        "name": s("Name", "Unique. Alerts and `POST /drift/run?check=<name>` use it.",
                  pattern="^[a-z0-9][a-z0-9-]*$",
                  message={"pattern": "Must be lowercase letters, numbers, and hyphens."}),
        "match": values_map("Instance attributes", "Effective attributes of the instances to plan."),
        "schedule": s("Schedule", "Standard cron (minute, hour, day of month, month, day of week), or `@daily`, "
                      "`@hourly`, `@every 6h`. Empty runs the check only from `POST /drift/run`.",
                      examples=["0 6 * * *"]),
        "timezone": s("Time zone", "The time zone of the schedule.", default="UTC", **TIMEZONE),
        "mode": choice("When drift is found", [("detect", "Alert only"),
                                               ("propose", "Propose a deployment that removes it"),
                                               ("resolve", "Deploy now to remove it")], default="detect"),
        "resolve_with": choice("Restore", [("deployed", "The last completed deployment"),
                                           ("saved", "The saved configuration, with the current bundle version")],
                               default="deployed"),
        "max_concurrent": i("Plans at once", "The rest wait in a queue.", default=5, minimum=1, maximum=50),
        "notify_clean": b("Also post instances without drift"),
        "summary": b("Post totals when the run finishes"),
        "slack": slack("Replaces the default channel for this check."),
        "patterns": obj("Plan log patterns", {
            "clean": s("No changes", default=r"No changes\."),
            "drift": s("Drift", default=r"Plan: \d+ to \w+(?:, \d+ to \w+)*"),
            "resource": s("Changed resource", default=r"(?m)^\s*# \S+.* (?:will be|must be) .*$"),
        }, "Regular expressions (Go RE2) that read the plan logs. Change them only for a provisioner whose plan "
           "output differs."),
    }, required=["name", "match", "mode", "resolve_with"])
    extra = {"attributes": ATTRIBUTES}
    if k8s:
        extra = {"storage_size_gi": storage("State storage (GiB)", "The volume for the plans in progress and the "
                                            "queues."), **extra}
    out["drift"] = dict(
        title="Drift detection",
        description="Plans the instances of each check on a schedule, and alerts Slack when the cloud no longer "
                    "matches the last deployment. Set the `DRIFT_RUN_TOKEN` secret to start checks with "
                    "`POST https://drift.<domain>/drift/run?check=<name>`.",
        config=obj("Checks", {
            "slack": slack("The channel for checks that do not name their own. Empty writes alerts to the logs "
                           "only."),
            "checks": arr("Checks", check, minItems=1, default=[
                {"name": "prod-nightly", "schedule": "0 6 * * *", "timezone": "UTC",
                 "match": {"md-environment": "prod"}, "mode": "detect", "resolve_with": "deployed"}]),
            "massdriver": massdriver_ui(),
        }, required=["checks"]),
        extra=extra,
        ui={"ui:order": ["slack", "checks", "massdriver"],
            "checks": {"ui:options": {"itemTitle": "{{name}}"}, "items": {
                "ui:order": ["name", "match", "schedule", "timezone", "mode", "resolve_with", "slack",
                             "notify_clean", "summary", "max_concurrent", "patterns"],
                "schedule": {"ui:placeholder": "0 6 * * *"},
                "mode": {"ui:widget": "radio"},
                "resolve_with": {"ui:widget": "radio"}}}},
    )

    # ---------------------------------------------------------------- freeze-guard
    actions = arr("Denied actions", {"type": "string", "oneOf": [{"const": v, "title": t} for v, t in
                                                                   DEPLOY_ACTIONS]},
                  uniqueItems=True, minItems=1)
    window = obj("Window", {
        "name": s("Name", "Unique. `POST /freeze/lift?window=<name>` uses it.", pattern="^[a-z0-9][a-z0-9-]*$",
                  message={"pattern": "Must be lowercase letters, numbers, and hyphens."}),
        "match": values_map("Attributes", "Attributes of the instances and environments to freeze. `*` matches "
                            "any value. Empty freezes everything."),
        "weekly": arr("Every week", obj("Range", {
            "from": s("From", pattern=r"^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) [0-2][0-9]:[0-5][0-9]$",
                      examples=["Fri 17:00"], message={"pattern": "Must be a day and a time, such as Fri 17:00."}),
            "to": s("To", pattern=r"^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) [0-2][0-9]:[0-5][0-9]$",
                    examples=["Mon 08:00"], message={"pattern": "Must be a day and a time, such as Mon 08:00."}),
        }, required=["from", "to"]), "Ranges from a day and time to a day and time. A range that ends before it "
                                     "starts wraps past Sunday.", default=[]),
        "from": s("Once, from", "A fixed window instead of a weekly one: `YYYY-MM-DD`, `YYYY-MM-DD HH:MM`, or "
                  "RFC 3339.", examples=["2026-12-24"]),
        "to": s("Once, to", examples=["2026-12-26 12:00"]),
        "timezone": s("Time zone", "Replaces the default time zone for this window.", **TIMEZONE),
        "exempt_groups": arr("More exempt groups", GROUPS, uniqueItems=True, default=[]),
        "actions": {**actions, "title": "Denied actions", "description": "Replaces the default actions for this "
                    "window."},
        "slack": slack("Replaces the default channel for this window."),
    }, required=["name"])
    extra = {}
    if k8s:
        extra = {"storage_size_gi": storage("State storage (GiB)", "The volume for the ids of the policies that "
                                            "freeze-guard created, and the lifts.")}
    out["freeze-guard"] = dict(
        title="Change freezes",
        description="During each window, adds deny policies for deployments to every custom group except the "
                    "exempt groups, and removes them when it ends. Set the `FREEZE_GUARD_ADMIN_TOKEN` secret to "
                    "lift a window early with `POST https://freeze-guard.<domain>/freeze/lift?window=<name>`.",
        config=obj("Windows", {
            "timezone": s("Default time zone", default="UTC", **TIMEZONE),
            "exempt_groups": arr("Exempt groups", GROUPS, "Groups that every window leaves alone, such as the "
                                 "on-call team.", uniqueItems=True, default=[]),
            "actions": {**actions, "default": [v for v, _ in DEPLOY_ACTIONS]},
            "slack": slack("The channel for window starts, ends, and lifts. Empty posts nothing."),
            "windows": arr("Windows", window, default=[]),
        }),
        extra=extra,
        ui={"ui:order": ["timezone", "exempt_groups", "actions", "slack", "windows"],
            "actions": {"ui:widget": "checkboxes"},
            "windows": {"ui:options": {"itemTitle": "{{name}}"}, "items": {
                "weekly": {"ui:options": {"itemTitle": "{{from}} to {{to}}"}},
                "ui:order": ["name", "match", "weekly", "from", "to", "timezone", "exempt_groups", "actions",
                             "slack"],
                "actions": {"ui:widget": "checkboxes"}}}},
    )

    # ---------------------------------------------------------------- github-deployments
    out["github-deployments"] = dict(
        title="GitHub Deployments",
        description="Shows Massdriver deployments as GitHub Deployments on the repository that an instance "
                    "names. Set the `GITHUB_DEPLOYMENTS_TOKEN` secret.",
        config=obj("GitHub", {
            "github": obj("GitHub", {
                "repo_attribute": s("Repository attribute", "The instance attribute that names the repository, as "
                                    "`owner/name`.", default="github_repo"),
                "ref_attribute": s("Ref attribute", "The instance attribute that names the ref. Without it, the "
                                   "deployment uses the default branch.", default="github_ref"),
                "environment_name": choice("GitHub environment",
                                           [("environment", "The environment, such as prod"),
                                            ("project-environment", "The project and environment, such as api-prod")],
                                           default="environment"),
                "api_url": s("API URL", "For GitHub Enterprise Server: `https://<host>/api/v3`.",
                             default="https://api.github.com", **HTTPS),
            }),
            "massdriver": massdriver_ui(),
        }),
        extra={},
        ui={"github": {"ui:order": ["repo_attribute", "ref_attribute", "environment_name", "api_url"]}},
    )

    # ---------------------------------------------------------------- proposal-chatops
    out["proposal-chatops"] = dict(
        title="Slack approvals",
        description="Posts each proposed deployment to Slack with Approve and Reject buttons. Needs the "
                    "`SLACK_BOT_TOKEN` and `SLACK_SIGNING_SECRET` secrets. Set the Slack app's interactivity URL "
                    "to `https://proposal-chatops.<domain>/slack/interactions`.",
        config=obj("Slack", {
            "slack": obj("Slack", {
                "channel": s("Channel", "The channel for proposals. Invite the app to a private channel.",
                             default="#deploys", **CHANNEL),
                "channel_attribute": s("Channel attribute", "An instance attribute whose value names the channel "
                                       "for its proposals.", examples=["slack_channel"]),
                "allowed_users": arr("Allowed users", s("Slack user ID", pattern="^[UW][A-Z0-9]+$",
                                                        message={"pattern": "Must be a Slack user ID, such as "
                                                                            "U012ABCDEF."}),
                                     "Slack user IDs that can approve and reject. Empty allows everybody in the "
                                     "channel.", default=[]),
            }, required=["channel"]),
        }, required=["slack"]),
        extra={"attributes": ATTRIBUTES},
        ui={"slack": {"ui:order": ["channel", "channel_attribute", "allowed_users"], "allowed_users": TAG}},
    )

    # ---------------------------------------------------------------- release-notes
    out["release-notes"] = dict(
        title="Release notes",
        description="Posts each new bundle version, and the instances that can upgrade, to Slack or Microsoft "
                    "Teams.",
        config=obj("Posts", {
            "slack": slack("The channel for release notes. Empty posts nothing to Slack."),
            "teams": obj("Microsoft Teams", {"enabled": b(
                "Post to Teams", "Posts an Adaptive Card to the webhook in the `RELEASE_NOTES_TEAMS_WEBHOOK_URL` "
                "secret.")}),
            "include_development": b("Include development versions",
                                     "Also post versions such as `1.3.1-dev.20261124T055403Z`."),
        }),
        extra={},
        ui={"ui:order": ["slack", "teams", "include_development"]},
    )

    # ---------------------------------------------------------------- rollback-on-failure
    out["rollback-on-failure"] = dict(
        title="Rollback on failure",
        description="When a deployment of an opted-in instance fails, deploys its last completed deployment again.",
        config=obj("Rollback", {
            "attribute": obj("Opt-in attribute", {
                "key": s("Key", default="autorollback"),
                "value": s("Value", default="true"),
            }, "Instances with this attribute roll back. The key must be a component custom attribute that allows "
               "the value.", required=["key", "value"]),
            "slack": slack("The channel for rollback notices. The result is a reply in the notice's thread. Empty "
                           "posts nothing."),
            "massdriver": massdriver_ui(),
        }, required=["attribute"]),
        extra={"attributes": ATTRIBUTES},
        ui={"ui:order": ["attribute", "slack", "massdriver"]},
    )

    # ---------------------------------------------------------------- router
    out["router"] = dict(
        title="Router",
        description="Runs HTTP requests, commands, and Massdriver API calls for the events that match its routes. "
                    "The event destination sends the event types of every route. The `ROUTER_ENV` secret is a "
                    "JSON object of extra environment variables, for `env.NAME` in jq or `{{ env \"NAME\" }}` in Go "
                    "templates.",
        config=s("Routes (YAML)", f"The YAML config of router. See the [router README]({README.format('router')}).",
                 default="routes:\n  - name: log-failures\n    events: [deployment.failed]\n    do:\n      - http:\n"
                         "          url: https://example.com/hooks/massdriver-failures\n"),
        extra={"scripts": {
            "type": "object", "title": "Scripts",
            "description": "Files for `exec` steps, by file name, mounted as executables in `/scripts`. A route "
                           "with an `exec` step, or any script, runs the `-scripts` image, which has `sh`, `jq`, "
                           "`curl`, and `python3`.",
            "default": {}, "propertyNames": {"pattern": "^[A-Za-z0-9][A-Za-z0-9._-]*$"},
            "additionalProperties": {"type": "string"}}},
        ui=None,
        config_ui={"ui:widget": "textarea", "ui:options": {"rows": 16}},
        extra_ui={"scripts": {"additionalProperties": {"ui:widget": "textarea", "ui:options": {"rows": 8}}}},
    )

    # ---------------------------------------------------------------- sandbox-on-join
    out["sandbox-on-join"] = dict(
        title="Sandboxes for new members",
        description="Forks a template environment for each person who joins a group, and removes it when they "
                    "leave.",
        config=obj("Sandboxes", {
            "sandboxes": arr("Rules", obj("Rule", {
                "group": {"type": "string", "title": "Group", "description": "Each person who joins it gets a sandbox.",
                          "$md.list": {"resource": "group"}},
                "project": {"type": "string", "title": "Project",
                            "description": "The project of the template. Forks go into the same project.",
                            "$md.list": {"resource": "project"}},
                "environment": s("Template environment"),
                "deploy": b("Deploy the fork after it is created"),
                "deleteAfterDecommission": b("Delete the fork after its decommission, when the person leaves"),
                "copyEnvironmentDefaults": b("Copy the template's environment defaults"),
                "copySecrets": b("Copy the secrets of the template's instances"),
                "copyRemoteReferences": b("Copy the template's remote references"),
            }, required=["group", "project", "environment"]), minItems=1),
        }, required=["sandboxes"]),
        extra={},
        ui={"sandboxes": {"ui:options": {"itemTitle": "{{project}}-{{environment}}"}, "items": {"ui:order": [
            "group", "project", "environment", "deploy", "deleteAfterDecommission", "copyEnvironmentDefaults",
            "copySecrets", "copyRemoteReferences"]}}},
    )

    # ---------------------------------------------------------------- statuspage
    impact = choice("Impact", [("degraded", "Degraded performance"), ("partial_outage", "Partial outage"),
                               ("major_outage", "Major outage")],
                    "How a firing alarm of an instance in this row shows.", default="major_outage")
    component = obj("Row", {
        "title": s("Title"),
        "description": s("Description"),
        "match": string_map("Instance attributes", "The instances in this row, such as `md-component` = `api`. "
                            "An instance belongs to the first row it matches."),
        "impact": impact,
    }, required=["title", "match"])
    environment = obj("Environment", {
        "id": s("Environment", "The environment as `<project>-<environment>`, such as `api-prod`.",
                pattern="^[a-z0-9]+-[a-z0-9]+$",
                message={"pattern": "Must be <project>-<environment>, such as api-prod."}),
        "title": s("Title"),
        "description": s("Description"),
        "components": arr("Rows", component, "Rows appear in this order. Empty shows one row for each instance.",
                          default=[]),
    }, required=["id", "title"])
    page = obj("Page", {
        "title": s("Title"),
        "slug": s("Slug", "The page's path. The first page is also served at `/`.", pattern="^[a-z0-9][a-z0-9-]*$",
                  message={"pattern": "Must be lowercase letters, numbers, and hyphens."}),
        "description": s("Description"),
        "environments": arr("Environments", environment, minItems=1),
    }, required=["title", "environments"])
    extra = {"host": s("Host", "The public host of the status page, such as `status.acme.com`. Empty uses "
                       "`statuspage.<domain>`.")}
    if k8s:
        extra["storage_size_gi"] = storage("History storage (GiB)", "The volume for incident and uptime history.")
        extra["output_size_gi"] = storage("Site storage (GiB)", "The volume mounted at the output directory, when "
                                          "it is set. Other pods on the same node can mount its claim, "
                                          "`<instance>-statuspage-site`, to serve or copy the files.")
    extra["attributes"] = ATTRIBUTES
    out["statuspage"] = dict(
        title="Status page",
        description="Serves a public status page from the alarms, instances, and deployments of the environments "
                    "on its pages.",
        config=obj("Status page", {
            "site": obj("Site", {
                "title": s("Title", default="Status"),
                "base_url": s("Base URL", "Absolute URLs in the Atom feed and `status.json` start with this.",
                              examples=["https://status.acme.com"], **HTTPS),
                "logo": s("Logo URL", **HTTPS),
                "accent_color": s("Accent color", default="#2563eb", pattern="^#[0-9a-fA-F]{3,8}$",
                                  message={"pattern": "Must be a hex color, such as #2563eb."}),
                "timezone": s("Time zone", "Dates and the day boundaries of the uptime bars use this zone.",
                              default="UTC", **TIMEZONE),
                "show_alarm_details": b("Show alarm names and messages",
                                        "They can name internal resources."),
                "footer_links": arr("Footer links", obj("Link", {"title": s("Title"), "url": s("URL", **HTTPS)},
                                                        required=["title", "url"]), default=[]),
            }, required=["title"]),
            "pages": arr("Pages", page, minItems=1, default=[
                {"title": "Status", "slug": "status", "environments": [{"id": "api-prod", "title": "Production",
                                                                        "components": []}]}]),
            "history_days": i("History (days)", "Days of uptime bars and incident history.", default=90,
                              minimum=1, maximum=365),
            "min_duration": s("Minimum alarm duration", "An alarm must fire this long to count as an incident.",
                              default="2m", **DURATION),
            "deploys_as_maintenance": b("Show deployments as maintenance",
                                        "A running deployment shows its row as under maintenance.", default=True),
            "announcements": arr("Announcements", obj("Announcement", {
                "message": s("Message"),
                "level": choice("Level", [("info", "Info"), ("warning", "Warning"), ("critical", "Critical")],
                                default="info"),
                "url": s("Link", **HTTPS),
                "pages": arr("Pages", s("Page slug"), "Empty shows it on every page.", default=[]),
                "starts_at": s("Starts", format="date-time"),
                "ends_at": s("Ends", format="date-time"),
            }, required=["message", "level"]), default=[]),
            "maintenance": arr("Scheduled maintenance", obj("Maintenance", {
                "title": s("Title"),
                "description": s("Description"),
                "starts_at": s("Starts", format="date-time"),
                "ends_at": s("Ends", format="date-time"),
                "environments": arr("Environments", s("Environment", examples=["api-prod"]), minItems=1),
                "components": arr("Rows", s("Row title"), "Empty covers the whole environment.", default=[]),
            }, required=["title", "starts_at", "ends_at", "environments"]), default=[]),
            "attributes": obj("Instance attributes", {
                "title": s("Title attribute", "Names the row of an instance in an environment without rows.",
                           default="status_title"),
                "hidden": s("Hidden attribute", "`true` hides the instance.", default="status_hidden"),
                "impact": s("Impact attribute", "Sets the impact of the instance's alarms.",
                            default="status_impact"),
            }),
            "output_dir": s("Output directory", "Writes the static site here after each change, to serve or sync "
                            "elsewhere. Empty writes no files.", pattern="^(/.+)?$"),
        }, required=["site", "pages"]),
        extra=extra,
        ui={"ui:order": ["site", "pages", "history_days", "min_duration", "deploys_as_maintenance",
                         "announcements", "maintenance", "attributes", "output_dir"],
            "site": {"ui:order": ["title", "base_url", "logo", "accent_color", "timezone", "show_alarm_details",
                                  "footer_links"],
                     "accent_color": {"ui:widget": "color"},
                     "footer_links": {"ui:options": {"itemTitle": "{{title}}"}}},
            "pages": {"ui:options": {"itemTitle": "{{title}}"}, "items": {"ui:order": ["title", "slug", "description", "environments"],
                                "description": {"ui:widget": "textarea", "ui:options": {"rows": 2}},
                                "environments": {"ui:options": {"itemTitle": "{{title}} ({{id}})"}, "items": {
                                    "ui:order": ["id", "title", "description", "components"],
                                    "components": {"ui:options": {"itemTitle": "{{title}}"}, "items": {"ui:order": ["title", "description", "match",
                                                                          "impact"]}}}}}},
            "announcements": {"ui:options": {"itemTitle": "{{message}}"},
                              "items": {"message": {"ui:widget": "textarea", "ui:options": {"rows": 2}},
                                        "pages": TAG}},
            "maintenance": {"ui:options": {"itemTitle": "{{title}}"},
                            "items": {"description": {"ui:widget": "textarea", "ui:options": {"rows": 2}},
                                      "environments": TAG, "components": TAG}}},
    )
    return out


def param_key(name):
    return name.replace("-", "_")


def build(k8s):
    params, ui = {}, {}
    for name, a in automations(k8s).items():
        on = {"enabled": {"const": True}, "config": a["config"], **a["extra"]}
        required = ["config"] + [k for k, v in a["extra"].items()
                                 if k in ("attributes", "storage_size_gi", "output_size_gi", "scripts")]
        params[param_key(name)] = {
            "type": "object",
            "title": a["title"],
            "description": f"{a['description']} [Docs]({README.format(name)})",
            "required": ["enabled"],
            "properties": {"enabled": b(f"Run {name}")},
            "dependencies": {"enabled": {"oneOf": [
                {"properties": {"enabled": {"const": False}}},
                {"properties": on, "required": required},
            ]}},
        }
        u = {"ui:order": ["enabled", "*"]}
        if a.get("ui"):
            u["config"] = a["ui"]
        if a.get("config_ui"):
            u["config"] = a["config_ui"]
        if a.get("extra_ui"):
            u.update(a["extra_ui"])
        if "attributes" in a["extra"]:
            u["attributes"] = {"ui:options": {"addButtonText": "Add attribute"}}
        ui[param_key(name)] = u
    return params, ui


class Dumper(yaml.SafeDumper):
    def ignore_aliases(self, data):
        return True


def str_rep(dumper, data):
    if "\n" in data:
        return dumper.represent_scalar("tag:yaml.org,2002:str", data, style="|")
    return dumper.represent_scalar("tag:yaml.org,2002:str", data)


Dumper.add_representer(str, str_rep)


def dump(d, indent):
    text = yaml.dump(d, Dumper=Dumper, sort_keys=False, width=116, allow_unicode=True)
    pad = " " * indent
    return "".join(pad + line if line.strip() else line for line in text.splitlines(True))


def splice(path, k8s):
    text = open(path).read()
    params, ui = build(k8s)
    p_start = text.index("    audit_archiver:\n")
    p_end = text.index("    otel:\n")
    text = text[:p_start] + dump(params, 4) + "\n" + text[p_end:]
    u_start = text.index("  audit_archiver:\n", text.index("\nui:\n"))
    u_end = text.index("  otel:\n", u_start)
    text = text[:u_start] + dump(ui, 2) + text[u_end:]
    open(path, "w").write(text)


if __name__ == "__main__":
    splice(f"{REPO}/bundles/massdriver-automations/massdriver.yaml", True)
    splice(f"{REPO}/bundles/massdriver-automations-ecs/massdriver.yaml", False)
