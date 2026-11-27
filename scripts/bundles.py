"""Writes one Massdriver bundle for each automation and platform under bundles/: massdriver-<automation> for
Kubernetes and massdriver-<automation>-ecs for Amazon ECS. Each bundle's src/ is the platform's files from
bundles/_shared/<platform>/ and the automation's file from bundles/_shared/automations/.

    python3 scripts/bundles.py

Run `mass bundle build` in each bundle afterward, to write its _massdriver_variables.tf.
"""
import os
import shutil

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
            "splunk": sink("Splunk", "A Splunk HTTP Event Collector. The `SPLUNK_HEC_TOKEN` secret "
                           "holds the token.", {
                "url": s("Collector URL", examples=["https://splunk.example.com:8088"], **HTTPS),
                "index": s("Index", "Empty uses the default index of the token."),
                "source": s("Source", default="massdriver"),
                "sourcetype": s("Source type", default="massdriver:cloudevent"),
            }, ["url"]),
            "datadog": sink("Datadog", "The Datadog Logs intake. The `DD_API_KEY` secret holds the "
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

    # ---------------------------------------------------------------- chaos-monkey
    out["chaos-monkey"] = dict(
        title="Chaos monkey",
        description="Picks matching instances at random on a schedule and sends them deployments of presets, "
                    "merged presets, mutated params, and gibberish, then posts how each one went. Set the "
                    "`CHAOS_MONKEY_RUN_TOKEN` secret to start runs with `POST https://<host>/chaos-monkey/run`.",
        config=obj("Chaos", {
            "match": values_map("Instance attributes", "Effective attributes of the instances to target. Match "
                                "only instances you can rebuild."),
            "mode": choice("Mode", [("propose", "Propose deployments for a person to approve"),
                                    ("plan", "Plan deployments, which change nothing"),
                                    ("deploy", "Deploy now")], default="propose"),
            "schedule": s("Schedule", "Standard cron (minute, hour, day of month, month, day of week), or "
                          "`@hourly`, `@every 30m`. Empty runs only from `POST /chaos-monkey/run`.",
                          examples=["@every 30m"]),
            "timezone": s("Time zone", "The time zone of the schedule.", default="UTC", **TIMEZONE),
            "status": arr("Instance statuses", {"type": "string", "enum": ["INITIALIZED", "PROVISIONED", "FAILED",
                                                                           "DECOMMISSIONED"]},
                          uniqueItems=True, minItems=1, default=["PROVISIONED"]),
            "instances": i("Instances per run", default=1, minimum=1, maximum=50),
            "strategies": obj("Strategy weights", {
                "preset": i("Preset", "One preset over the instance's params.", default=1, minimum=0),
                "merge": i("Merge", "Two or more presets merged together.", default=1, minimum=0),
                "mutate": i("Mutate", "Random fields set to new values from their schema.", default=1, minimum=0),
                "gibberish": i("Gibberish", "Random fields set to nonsense.", default=1, minimum=0),
            }, "How often each strategy is picked. 0 turns one off."),
            "mutate": obj("Mutate", {"rate": {"type": "number", "title": "Rate", "description": "The chance that "
                                              "each field changes.", "default": 0.3, "minimum": 0.01,
                                              "maximum": 1}}),
            "gibberish": obj("Gibberish", {"max_fields": i("Most fields", "The most fields that one run garbles.",
                                                           default=3, minimum=1, maximum=50)}),
            "sign_off": b("Sign off", "Ends each deployment message and its Slack posts with a signature.",
                          default=True),
            "signature": s("Signature", "Empty picks one of ten built-in sign-offs for each deployment.",
                           examples=["Hey, thanks for monkeyin' around. 🐒🤙🏼🏄‍♂️"]),
            "slack": slack("The channel for what each run sent and how each deployment ended. Empty writes them "
                           "to the logs only."),
            "massdriver": massdriver_ui(),
        }, required=["match", "mode"]),
        extra={},
        ui={"ui:order": ["match", "mode", "schedule", "timezone", "status", "instances", "strategies", "mutate",
                         "gibberish", "sign_off", "signature", "slack", "massdriver"],
            "status": {"ui:widget": "checkboxes", "ui:options": {"inline": True}}},
    )

    # ---------------------------------------------------------------- cost-explorer
    extra = {}
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
                    "`POST https://<host>/drift/run?check=<name>`.",
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
                    "exempt groups, and removes them when it ends. Set the `FREEZE_ADMIN_TOKEN` secret to "
                    "lift a window early with `POST https://<host>/freeze/lift?window=<name>`.",
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
                    "names. Set the `GITHUB_TOKEN` secret.",
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

    # ---------------------------------------------------------------- pr-sync
    out["pr-sync"] = dict(
        title="Pull requests in the project stash",
        description="Writes the pull requests of a project's GitHub repositories to a JSON entry in the project's "
                    "stash on an interval, so a note on the project lists their review, check, and merge state. Set "
                    "the `GITHUB_TOKEN` secret.",
        config=obj("Pull requests", {
            "projects": arr("Projects", obj("Project", {
                "project": {"type": "string", "title": "Project", "description": "The project whose stash gets the "
                            "entry.", "$md.list": {"resource": "project"}},
                "repos": arr("Repositories", s("Repository", pattern=r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$",
                                               message={"pattern": "Must be owner/name, such as acme/api."}),
                             "GitHub repositories, as `owner/name`.", minItems=1),
            }, required=["project", "repos"]), minItems=1),
            "interval": s("Interval", "How often to read GitHub. At least 30s.", default="2m", **DURATION),
            "merged_window": s("Merged window", "How long a merged pull request stays in the list. `0s` lists only "
                               "open pull requests.", default="24h", **DURATION),
            "max_prs": i("Most pull requests", "The most pull requests in one project's entry.", default=20,
                         minimum=1, maximum=50),
            "key": s("Stash key", "The key in each project's stash.", default="github/prs",
                     pattern=r"^[A-Za-z0-9_./-]{1,255}$",
                     message={"pattern": "Must be letters, digits, and the characters _ . / -."}),
            "label": s("Label", "The entry's name in the Massdriver UI.", default="Pull requests"),
            "github": obj("GitHub", {
                "graphql_url": s("GraphQL URL", "For GitHub Enterprise Server: `https://<host>/api/graphql`.",
                                 default="https://api.github.com/graphql", **HTTPS),
            }),
        }, required=["projects"]),
        extra={},
        ui={"ui:order": ["projects", "interval", "merged_window", "max_prs", "key", "label", "github"],
            "projects": {"ui:options": {"itemTitle": "{{project}}"},
                         "items": {"ui:order": ["project", "repos"], "repos": TAG}}},
    )

    # ---------------------------------------------------------------- proposal-chatops
    out["proposal-chatops"] = dict(
        title="Slack approvals",
        description="Posts each proposed deployment to Slack with Approve and Reject buttons. Needs the "
                    "`SLACK_BOT_TOKEN` and `SLACK_SIGNING_SECRET` secrets. Set the Slack app's interactivity URL "
                    "to `https://<host>/slack/interactions`.",
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
                "Post to Teams", "Posts an Adaptive Card to the webhook in the `TEAMS_WEBHOOK_URL` "
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

    # ---------------------------------------------------------------- stale-proposals
    wait = dict(pattern=r"^([0-9]+(\.[0-9]+)?d|([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+)$",
                message={"pattern": "Must be a duration, such as 36h or 2d."})
    out["stale-proposals"] = dict(
        title="Stale proposals",
        description="Posts a digest of the proposed deployments that have waited too long for approval, with Slack "
                    "mentions for the people who can approve them, and can reject the oldest. Set the "
                    "`STALE_PROPOSALS_RUN_TOKEN` secret to start runs with `POST https://<host>/stale-proposals/run`.",
        config=obj("Digest", {
            "schedule": s("Schedule", "Standard cron (minute, hour, day of month, month, day of week), or "
                          "`@daily`. Empty runs only from `POST /stale-proposals/run`.",
                          default="0 9 * * MON-FRI"),
            "timezone": s("Time zone", "The time zone of the schedule.", default="UTC", **TIMEZONE),
            "remind_after": s("Remind after", "How long a proposal waits before it is in the digest.",
                              default="24h", **wait),
            "reject_after": s("Reject after", "Reject proposals that have waited this long. Empty rejects none.",
                              examples=["14d"], **wait),
            "dry_run": b("Dry run", "List what would be rejected, and reject nothing."),
            "group_by": choice("Group by", [("environment", "Project and environment"), ("project", "Project")],
                               default="environment"),
            "match": values_map("Instance attributes", "Effective attributes that a proposal's instance must "
                                "have. Empty matches every proposal."),
            "exclude": values_map("Skip attributes", "Skip the proposals whose instance has every one of these "
                                  "attributes."),
            "mentions": arr("Mentions", obj("Mention rule", {
                "match": values_map("Instance attributes", "The proposals that get these mentions. Empty "
                                    "matches every proposal."),
                "mentions": arr("Mentions", s("Mention", examples=["<!subteam^S0123456789>"]), minItems=1),
            }, required=["mentions"]), "Slack mentions, such as `<@U123>`, `<!subteam^S123>`, or `<!here>`, "
               "added to the proposals each rule matches.", default=[]),
            "slack": slack("The channel for the digest. Empty writes it to the logs only."),
            "massdriver": massdriver_ui(),
        }),
        extra={},
        ui={"ui:order": ["schedule", "timezone", "remind_after", "reject_after", "dry_run", "group_by", "match",
                         "exclude", "mentions", "slack", "massdriver"],
            "mentions": {"items": {"ui:order": ["match", "mentions"], "mentions": TAG}}},
    )

    # ---------------------------------------------------------------- stash-sync
    aws_creds = ("The `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` secrets hold the AWS keys for CloudWatch."
                 if k8s else "CloudWatch rules and presets use a task role that can read metrics and alarms, unless "
                 "the `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` secrets are set.")
    out["stash-sync"] = dict(
        title="Stash sync",
        description="Copies CloudWatch and Prometheus metrics into Massdriver stash entries on an interval, so "
                    "widgets show health, usage, traffic, and banners. In simulate mode it writes changing values "
                    "from a scenario, for demos. " + aws_creds,
        config=s("Settings (YAML)", f"The YAML config of stash-sync. See the [stash-sync README]"
                 f"({README.format('stash-sync')}). Set `prometheus.token_env: PROMETHEUS_TOKEN` or "
                 "`prometheus.password_env: PROMETHEUS_PASSWORD` to use the Prometheus secrets.",
                 default="interval: 60s\npresets:\n  - preset: aws-alarm-health\n    instance: api-prod-database\n"
                         "    with:\n      alarm: api-prod-database-cpu\n"),
        extra={},
        ui=None,
        config_ui={"ui:widget": "textarea", "ui:options": {"rows": 16}},
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
    extra = {}
    if k8s:
        extra["storage_size_gi"] = storage("History storage (GiB)", "The volume for incident and uptime history.")
        extra["site_size_gi"] = storage("Site storage (GiB)", "The volume mounted at the output directory, when "
                                          "it is set. Other pods on the same node can mount its claim, "
                                          "`<instance>-site`, to serve or copy the files.")
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

    # ---------------------------------------------------------------- upgrade-train
    stage = obj("Stage", {
        "name": s("Name", "Unique in the train.", pattern="^[a-z0-9][a-z0-9-]*$",
                  message={"pattern": "Must be lowercase letters, numbers, and hyphens."}),
        "match": values_map("Instance attributes", "Effective attributes of the instances in this stage."),
        "mode": choice("Mode", [("deploy", "Deploy now"), ("propose", "Propose a deployment for a person to approve")],
                       default="propose"),
        "soak": s("Soak", "How long the stage waits after its deployments complete, with no firing alarms, before "
                  "the next stage starts.", default="1h", **DURATION),
    }, required=["name", "match", "mode"])
    train = obj("Train", {
        "name": s("Name", "Unique. Posts, deployment messages, and `POST /upgrade-train/run?train=<name>` use it.",
                  pattern="^[a-z0-9][a-z0-9-]*$",
                  message={"pattern": "Must be lowercase letters, numbers, and hyphens."}),
        "bundles": arr("Bundles", s("Bundle", examples=["aws-aurora-postgres"]),
                       "The bundles whose new versions ride this train. Each bundle runs on its own.",
                       uniqueItems=True, minItems=1),
        "include_development": b("Include development versions",
                                 "Also run versions such as `1.3.1-dev.20261124T055403Z`."),
        "stages": arr("Stages", stage, "Run in order.", minItems=1),
        "slack": slack("Replaces the default channel for this train."),
    }, required=["name", "bundles", "stages"])
    extra = {}
    if k8s:
        extra["storage_size_gi"] = storage("State storage (GiB)", "The volume for the runs in progress.")
    out["upgrade-train"] = dict(
        title="Upgrade train",
        description="Moves each new bundle version through stages of instances, such as dev, staging, and prod, "
                    "with a soak time and an alarm check between stages. Set the `UPGRADE_TRAIN_TOKEN` secret to "
                    "start, resume, and stop runs with `POST https://<host>/upgrade-train/run` and `/halt`.",
        config=obj("Trains", {
            "slack": slack("The channel for trains that do not name their own. Empty writes posts to the logs "
                           "only."),
            "trains": arr("Trains", train, minItems=1),
            "massdriver": massdriver_ui(),
        }, required=["trains"]),
        extra=extra,
        ui={"ui:order": ["slack", "trains", "massdriver"],
            "trains": {"ui:options": {"itemTitle": "{{name}}"},
                       "items": {"ui:order": ["name", "bundles", "include_development", "stages", "slack"],
                                 "bundles": TAG,
                                 "stages": {"ui:options": {"itemTitle": "{{name}}"},
                                            "items": {"ui:order": ["name", "match", "mode", "soak"]}}}}},
    )
    return out


STATEFUL = {"cost-explorer", "drift", "freeze-guard", "statuspage", "upgrade-train"}
WEBSITE = {"cost-explorer": "The cost dashboard.", "statuspage": "The status page."}

SECRET_DEFS = {
    "SLACK_BOT_TOKEN": {"title": "Slack bot token",
                        "description": "The `xoxb-` token of the Slack app. Posts go to the channels in the settings."},
    "SLACK_SIGNING_SECRET": {"title": "Slack signing secret",
                             "description": "The signing secret of the Slack app, to check button clicks."},
    "SLACK_WEBHOOK_URL": {"title": "Slack webhook URL",
                          "description": "An incoming webhook, for a workspace without the Slack app. Every post goes "
                                         "to its one channel. Ignored when the bot token is set."},
    "AWS_ACCESS_KEY_ID": {"title": "AWS access key ID"},
    "AWS_SECRET_ACCESS_KEY": {"title": "AWS secret access key"},
    "SPLUNK_HEC_TOKEN": {"title": "Splunk HEC token"},
    "DD_API_KEY": {"title": "Datadog API key"},
    "PROMETHEUS_TOKEN": {"title": "Prometheus token",
                         "description": "A bearer token for the Prometheus HTTP API, for `prometheus.token_env: "
                                        "PROMETHEUS_TOKEN`."},
    "PROMETHEUS_PASSWORD": {"title": "Prometheus password",
                            "description": "The basic auth password of `prometheus.username`, for "
                                           "`prometheus.password_env: PROMETHEUS_PASSWORD`."},
    "COST_EXPLORER_PASSWORD": {"title": "Dashboard password",
                               "description": "The basic auth password. The user is the `username` in the settings, "
                                              "`massdriver` by default."},
    "CHAOS_MONKEY_RUN_TOKEN": {"title": "Run token",
                               "description": "Callers of `POST /chaos-monkey/run` send it as "
                                              "`Authorization: Bearer <token>`."},
    "STALE_PROPOSALS_RUN_TOKEN": {"title": "Run token",
                                  "description": "Callers of `POST /stale-proposals/run` send it as "
                                                 "`Authorization: Bearer <token>`."},
    "DRIFT_RUN_TOKEN": {"title": "Run token",
                        "description": "Callers of `POST /drift/run` send it as `Authorization: Bearer <token>`."},
    "UPGRADE_TRAIN_TOKEN": {"title": "Run token",
                            "description": "Callers of `POST /upgrade-train/run` and `POST /upgrade-train/halt` send "
                                           "it as `Authorization: Bearer <token>`."},
    "FREEZE_ADMIN_TOKEN": {"title": "Admin token",
                           "description": "Callers of `POST /freeze/lift` send it as `Authorization: Bearer <token>`."},
    "GITHUB_TOKEN": {"title": "GitHub token",
                     "description": "Needs `Deployments` write and `Metadata` read, or the `repo_deployment` scope."},
    "TEAMS_WEBHOOK_URL": {"title": "Teams webhook URL",
                          "description": "A Microsoft Teams Workflows webhook, for `teams.enabled`."},
    "ROUTER_ENV": {"title": "Route environment",
                   "description": "A JSON object of environment variables for routes, such as "
                                  "`{\"PAGERDUTY_ROUTING_KEY\": \"...\"}`.", "json": True},
    "OTEL_EXPORTER_OTLP_HEADERS": {"title": "OTLP headers",
                                   "description": "Headers for the OTLP endpoint, as `key=value` pairs separated by "
                                                  "commas, such as `authorization=Bearer abc123`."},
}
# One secret name can need different access in different automations.
SECRET_OVERRIDES = {
    "pr-sync": {"GITHUB_TOKEN": {"title": "GitHub token",
                                 "description": "Needs `Pull requests`, `Commit statuses`, and `Contents` read, or "
                                                "the `repo` scope for private repositories."}},
}


def secret_def(name, k):
    return SECRET_OVERRIDES.get(name, {}).get(k, SECRET_DEFS[k])


SLACK = ["SLACK_BOT_TOKEN", "SLACK_WEBHOOK_URL"]
SECRETS = {
    "audit-archiver": ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "SPLUNK_HEC_TOKEN", "DD_API_KEY"],
    "bundle-scanner": SLACK,
    "chaos-monkey": SLACK + ["CHAOS_MONKEY_RUN_TOKEN"],
    "cost-explorer": ["COST_EXPLORER_PASSWORD"],
    "drift": SLACK + ["DRIFT_RUN_TOKEN"],
    "freeze-guard": SLACK + ["FREEZE_ADMIN_TOKEN"],
    "github-deployments": ["GITHUB_TOKEN"],
    "pr-sync": ["GITHUB_TOKEN"],
    "proposal-chatops": ["SLACK_BOT_TOKEN", "SLACK_SIGNING_SECRET"],
    "release-notes": SLACK + ["TEAMS_WEBHOOK_URL"],
    "rollback-on-failure": SLACK,
    "router": ["ROUTER_ENV"],
    "upgrade-train": SLACK + ["UPGRADE_TRAIN_TOKEN"],
    "stale-proposals": SLACK + ["STALE_PROPOSALS_RUN_TOKEN"],
    "stash-sync": ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "PROMETHEUS_TOKEN", "PROMETHEUS_PASSWORD"],
}

HOST = r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$"
HOST_MESSAGE = {"pattern": "Must be a lowercase DNS name, such as drift.automations.acme.com."}

IMAGE = obj("Image", {
    "repository": s("Repository", "The automations image. bundle-scanner adds `-scanner` to the tag, and router "
                    "adds `-scripts` when it runs commands.", default="ghcr.io/massdriver-cloud/massdriver-automations"),
    "tag": s("Tag", "The image tag, such as `0.2.0`. Pin a version so that a redeploy runs the same code.",
             default="0.2", pattern="^[A-Za-z0-9_][A-Za-z0-9_.-]{0,100}$",
             message={"pattern": "Must be a valid image tag."}),
}, required=["repository", "tag"])

OTEL = obj("Tracing", {"endpoint": s(
    "OTLP endpoint", "The OTLP/HTTP base URL of a collector, such as `http://otel-collector.observability:4318`. "
    "Empty turns tracing off.")}, "Sends OpenTelemetry traces to an OTLP endpoint. Set the "
    "`OTEL_EXPORTER_OTLP_HEADERS` secret when the endpoint needs credentials.")


def ingress(k8s, name):
    if k8s:
        return obj("Ingress", {
            "host": s("Host", f"Where the automation is served, such as `{name}.automations.acme.com`. Point it at "
                      "the ingress controller. Massdriver sends events to `https://<host>/hooks/massdriver`.",
                      pattern=HOST, message=HOST_MESSAGE),
            "class_name": s("Ingress class", "Empty uses the default class of the cluster."),
            "cluster_issuer": s("cert-manager ClusterIssuer", "Issues a certificate for the host. Leave empty when "
                                "TLS ends in front of the ingress controller, or when you set a TLS secret."),
            "tls_secret_name": s("TLS secret", "A Secret in the namespace with a certificate for the host. Ignored "
                                 "when a ClusterIssuer is set."),
        }, required=["host"])
    return obj("Ingress", {
        "host": s("Host", f"Where the automation is served. Empty uses `{name}.<domain of the cluster's hosted "
                  "zone>`. The load balancer's HTTPS listener needs a certificate for it. Massdriver sends events "
                  "to `https://<host>/hooks/massdriver`.", pattern=HOST, message=HOST_MESSAGE),
        "dns_records": b("Create a DNS record", "Creates a CNAME record to the load balancer when the host is in "
                         "the cluster's hosted zone.", default=True),
    }, required=["dns_records"])


LOGS = obj("Logs", {"retention_days": i("Retention (days)", "Days that CloudWatch Logs keeps the logs.", default=90,
                                        enum=[1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096,
                                              1827, 2192, 2557, 2922, 3288, 3653])}, required=["retention_days"])


def bundle_name(name, k8s):
    return f"massdriver-{name}" if k8s else f"massdriver-{name}-ecs"


def manifest(name, a, k8s):
    bname = bundle_name(name, k8s)
    platform = "a Kubernetes cluster" if k8s else "Amazon ECS with Fargate"
    props = {"image": IMAGE, "ingress": ingress(k8s, name)}
    if not k8s:
        props["logs"] = LOGS
    props["config"] = a["config"] if a["config"]["type"] != "object" else {**a["config"], "title": "Settings"}
    props.update(a["extra"])
    props["otel"] = OTEL
    required = [k for k in props if k not in ("otel", "scripts")]

    ui = {"ui:order": list(props) + ["*"], "image": {"ui:order": ["repository", "tag"]},
          "ingress": {"ui:order": list(props["ingress"]["properties"])}}
    if not k8s:
        ui["logs"] = {"ui:order": ["retention_days"]}
    if a.get("ui"):
        ui["config"] = a["ui"]
    if a.get("config_ui"):
        ui["config"] = a["config_ui"]
    if a.get("extra_ui"):
        ui.update(a["extra_ui"])
    if "attributes" in a["extra"]:
        ui["attributes"] = {"ui:options": {"addButtonText": "Add attribute"}}

    deps = ({"kubernetes_cluster": {"resource_type": "massdriver-kubernetes-cluster", "required": True}} if k8s else
            {"aws_authentication": {"resource_type": "aws-iam-role", "required": True},
             "ecs_cluster": {"resource_type": "aws-ecs-cluster", "required": True}})
    deps["service_account"] = {"resource_type": "massdriver-service-account", "required": True}

    secrets = {k: secret_def(name, k) for k in SECRETS.get(name, []) + ["OTEL_EXPORTER_OTLP_HEADERS"]}
    m = {
        "name": bname,
        "description": f"{a['description']} Runs on {platform}, with an event destination that sends it only the "
                       "events it uses.",
        "source_url": f"https://github.com/massdriver-cloud/massdriver-automations/tree/main/bundles/{bname}",
        "version": "0.1.0",
        "steps": [{"path": "src", "provisioner": "opentofu:1.10"}],
        "params": {"required": required, "properties": props},
        "dependencies": deps,
        "resources": {"website": {"resource_type": "website", "required": True}} if name in WEBSITE else {},
        "app": {"secrets": secrets},
        "ui": ui,
    }
    return m


def readme(name, a, k8s):
    bname = bundle_name(name, k8s)
    other = bundle_name(name, not k8s)
    platform = "a Kubernetes cluster" if k8s else "Amazon ECS with Fargate"
    lines = [
        f"# {bname}",
        "",
        f"Runs [{name}](../../automations/{name}) on {platform}. {a['description']}",
        "",
        f"The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run {name}`, serves "
        "it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` "
        "with only the events the automation uses. **Settings** has the keys of the automation's config file.",
        "",
        f"The same automation on {'Amazon ECS' if k8s else 'Kubernetes'} is [{other}](../{other}). See "
        f"[{'Kubernetes' if k8s else 'Amazon ECS'}](../README.md#{'kubernetes' if k8s else 'amazon-ecs'}) for what the "
        "bundle creates and needs.",
    ]
    if name in WEBSITE:
        lines += ["", f"It produces a `website` resource with the URL to open: {WEBSITE[name].lower()} The type is an "
                  "example in [examples/resource-types/website](../../examples/resource-types/website). Publish it "
                  "to your organization before you publish the bundle."]
    secrets = SECRETS.get(name, [])
    if secrets:
        lines += ["", "## Secrets", "", "| Secret | |", "|---|---|"]
        defs = {k: secret_def(name, k) for k in secrets}
        lines += [f"| `{k}` | {d.get('description', d['title'] + '.')} |" for k, d in defs.items()]
    return "\n".join(lines) + "\n"


def write_bundle(name, a, k8s):
    bname = bundle_name(name, k8s)
    root = os.path.join(REPO, "bundles", bname)
    src = os.path.join(root, "src")
    os.makedirs(src, exist_ok=True)
    platform = "kubernetes" if k8s else "ecs"
    keep = {"_massdriver_variables.tf"}
    wanted = set()
    for f in sorted(os.listdir(os.path.join(REPO, "bundles", "_shared", platform))):
        shutil.copyfile(os.path.join(REPO, "bundles", "_shared", platform, f), os.path.join(src, f))
        wanted.add(f)
    shutil.copyfile(os.path.join(REPO, "bundles", "_shared", "automations", f"{name}.tf"),
                    os.path.join(src, "automation.tf"))
    wanted.add("automation.tf")
    sizes = {"state_size_gi": "var.storage_size_gi" if k8s and name in STATEFUL else "1",
             "site_size_gi": "var.site_size_gi" if k8s and name == "statuspage" else "1"}
    with open(os.path.join(src, "sizes.tf"), "w") as f:
        width = max(len(k) for k in sizes)
        f.write("locals {\n" + "".join(f"  {k.ljust(width)} = {v}\n" for k, v in sizes.items()) + "}\n")
    wanted.add("sizes.tf")
    for f in os.listdir(src):
        if f.endswith(".tf") and f not in wanted | keep:
            os.remove(os.path.join(src, f))
    with open(os.path.join(root, "massdriver.yaml"), "w") as f:
        f.write(yaml.dump(manifest(name, a, k8s), Dumper=Dumper, sort_keys=False, width=116, allow_unicode=True))
    with open(os.path.join(root, "README.md"), "w") as f:
        f.write(readme(name, a, k8s))


class Dumper(yaml.SafeDumper):
    def ignore_aliases(self, data):
        return True


def str_rep(dumper, data):
    if "\n" in data:
        return dumper.represent_scalar("tag:yaml.org,2002:str", data, style="|")
    return dumper.represent_scalar("tag:yaml.org,2002:str", data)


Dumper.add_representer(str, str_rep)


if __name__ == "__main__":
    for k8s in (True, False):
        for name, a in automations(k8s).items():
            write_bundle(name, a, k8s)
