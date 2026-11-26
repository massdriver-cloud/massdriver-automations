# massdriver-automations-ecs bundle

Runs the [automations](../README.md) on Amazon ECS with Fargate. It has the same settings, secrets, and event
destinations as the [Kubernetes bundle](../bundle). Turn on any set of automations. For each enabled automation, the
bundle creates:

- an ECS service with one Fargate task (ARM64), running `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run <name>`
- a task definition with two containers: `files`, which writes the config file and the router **Scripts** to
  task volumes and exits, and the automation, which starts after `files` succeeds
- a Secrets Manager secret with the automation's secret environment variables
- a CloudWatch log group
- a task execution role that can read only the automation's secret and write only to its log group
- a target group with a health check on `/healthz`, and a rule on the cluster's HTTPS listener for the host
  `<name>.<domain>`
- a CNAME record for the host in the cluster's hosted zone, when the cluster has one and the host is in it
- a Massdriver event destination that sends events to `https://<name>.<domain>/hooks/massdriver`, with the event
  types, attributes, and condition from the automation's README
- for drift, freeze-guard, and statuspage, an EFS access point mounted at `STATE_DIR`, and a task role that can
  mount only that access point
- for statuspage with `output_dir` in its config, a second EFS access point mounted writable at `output_dir`
- for audit-archiver with an S3 bucket and no AWS key secrets, a task role that can write to the bucket

The instance also gets one KMS key for the secrets, the logs, and the state, one security group for the tasks, and,
when an automation keeps state, an EFS file system with a mount target in each Availability Zone of the subnets.
The bundle produces no resources for other bundles.

## Requirements

- The resource type `massdriver-service-account` installed in your organization from
  [massdriver-resource-types](https://github.com/massdriver-cloud/massdriver-resource-types).
- An `aws-iam-role` dependency with `arn` and `token_level`. The provider assumes the role with the deployment's
  Massdriver identity token of that level, so the role's trust policy must accept it.
- An `aws-ecs-cluster` dependency. This type is an example in
  [examples/resource-types/aws-ecs-cluster](../examples/resource-types/aws-ecs-cluster). Publish it, or a copy that
  you change, to your organization.
- A [`massdriver-service-account`](https://github.com/massdriver-cloud/massdriver-resource-types/tree/main/massdriver-service-account)
  dependency.
- An internet-facing Application Load Balancer whose listener, `load_balancer.listener_arn`, is HTTPS. The plan
  warns when `load_balancer.protocol` is not `HTTPS`, or when it is not set and the cluster has no `zone`. The
  listener needs a certificate for
  `*.<domain>`, and for the statuspage host when it is outside `<domain>`. Massdriver and Slack send requests to
  `https://` URLs.
- The load balancer's security group allows outbound traffic to the tasks on port 8080.
- Private subnets, `network.private_subnet_ids`, with a route through a NAT gateway, so tasks can pull images and
  call the Massdriver API, Slack, GitHub, and the other services in their settings. The plan warns when
  `network.internet_egress` is false.

## Ingress host

Set **Ingress > Domain**, such as `automations.acme.com`, or leave it empty to use the cluster's `zone.domain`. Each automation is served at `<name>.<domain>`:

| Automation | Host | Paths that must be public |
|---|---|---|
| audit-archiver | `audit-archiver.<domain>` | `/hooks/massdriver` |
| autodeploy-downstream | `autodeploy-downstream.<domain>` | `/hooks/massdriver` |
| bundle-scanner | `bundle-scanner.<domain>` | `/hooks/massdriver` |
| drift | `drift.<domain>` | `/hooks/massdriver`, and `/drift/run` to start checks from outside AWS |
| freeze-guard | `freeze-guard.<domain>` | `/hooks/massdriver`, and `/freeze/lift` to lift a freeze from outside AWS |
| github-deployments | `github-deployments.<domain>` | `/hooks/massdriver` |
| proposal-chatops | `proposal-chatops.<domain>` | `/hooks/massdriver`, `/slack/interactions` |
| release-notes | `release-notes.<domain>` | `/hooks/massdriver` |
| rollback-on-failure | `rollback-on-failure.<domain>` | `/hooks/massdriver` |
| router | `router.<domain>` | `/hooks/massdriver` |
| sandbox-on-join | `sandbox-on-join.<domain>` | `/hooks/massdriver` |
| statuspage | **statuspage > Host**, or `statuspage.<domain>` | every path |

Each listener rule sends every path of its host to the automation. Two instances of this bundle on one load balancer
need different domains, because each host serves one automation.

With **Ingress > Create DNS records** on, the bundle creates a CNAME record to the load balancer for each host that
ends in the base domain of the cluster's hosted zone. Point the other hosts at the load balancer yourself.

For proposal-chatops, set the Slack app's interactivity request URL to
`https://proposal-chatops.<domain>/slack/interactions`.

## Configuration files and secrets

The automations read their YAML configuration from a file. The image has no shell, and Fargate has no equivalent of
a ConfigMap, so each task starts a `files` container from the `<tag>-scripts` image. It writes each file from the
task definition to a task volume, which the automation mounts read-only at `/etc/massdriver-automations`, and the
router **Scripts** to `/scripts` with mode `0755`. A change to a file makes a new task definition revision, and the
service replaces its task.

Settings that are not secret are environment variables in the task definition. The secrets, the service account
token, and the signing secret of the event destination are keys of the automation's Secrets Manager secret, and ECS
passes each one to the container as an environment variable. The task definition names the secret version, so a
change to a secret also replaces the task.

## State

drift, freeze-guard, and statuspage keep state in `STATE_DIR`: plans in progress and queues, the ids of the policies
that freeze-guard created, and incident and uptime history. Each one gets an access point on the instance's EFS file
system, with the root directory `/<name>` and the user `65532`. EFS grows with the data, so there is no size
setting. Decommissioning the instance deletes the file system and the state.

When the statuspage config sets `output_dir`, such as `/srv/status`, statuspage writes the static site to an access
point of its own on the same file system, with the root directory `/statuspage-site`. To serve or copy the files,
mount that access point in another task or service, read-only. Its role needs `elasticfilesystem:ClientMount` on the
file system with the condition `elasticfilesystem:AccessPointArn` set to the access point, and its security group
needs to be allowed by the file system's security group on port 2049.

## Configuration

Each automation has one **Configuration** parameter: the YAML of its config file, as its README and its
`config.example.yaml` describe. The default is a small valid config to edit. The bundle writes it to
`/etc/massdriver-automations/config.yaml`, the default `CONFIG_FILE`, and the automation reads it at start. A change
restarts the automation. A config that does not decode or validate stops the automation with one `invalid config`
log record.

A key that names an environment variable for a credential, such as `github.token_env` or `splunk.token_env`, must
keep its default: the bundle sets only the variables in [Secrets](#secrets).

## Slack

The automations share one Slack app. Set the `SLACK_BOT_TOKEN` and `SLACK_SIGNING_SECRET` secrets from it, and set
`slack.channel` in the config of each automation that posts. Without the app, set the `SLACK_WEBHOOK_URL` secret:
every automation that posts then sends to the webhook's one channel. See [Slack](../README.md#slack).

## Service account

The bundle creates event destinations with the `massdriver-service-account` dependency, because a deployment cannot
create them. The automations that call the Massdriver API get its URL, organization, and token as `MASSDRIVER_URL`,
`MASSDRIVER_ORGANIZATION_ID`, and `MASSDRIVER_API_KEY`.

The service account needs `organization:manageEventDestinations`, plus the permissions of each automation that you
turn on:

| Automation | Permissions |
|---|---|
| audit-archiver | None. |
| autodeploy-downstream | `instance:deploy` on the downstream instances, `instance:propose` on those of `propose` rules, and `project:view` on their projects. |
| bundle-scanner | `repo:view` and `repo:pull` on the bundle repos. With a result attribute key, also `repo:update`. |
| drift | `project:view` and `instance:plan` on the matched instances. Mode `resolve` also needs `instance:deploy`, and `instance:propose` with `resolve_with: deployed`. Mode `propose` needs `instance:propose`. |
| freeze-guard | `organization:manageGroups`, to create and delete group policies. Any member of the organization can list groups and their policies. |
| github-deployments | None. |
| proposal-chatops | `instance:deploy` on the instances. |
| release-notes | `repo:view` on the bundle repos, and `project:view` on the projects whose instances are listed. |
| rollback-on-failure | `instance:propose` and `instance:deploy` on the opted-in instances, and `project:view` on their projects. |
| router | The permissions of the operations in its `massdriver` steps. None without them. |
| sandbox-on-join | `project:view` on the template's project, `environment:create` for environments with the attributes `owner` and `sandbox`, and `environment:deploy`, `environment:decommission`, and `environment:delete` on the forks. With the `copy*` options, also `resource:view`, a resource grant for each copied environment default and remote reference, and `instance:configure` on each template instance with secrets. |
| statuspage | `project:view` on the projects of the configured environments. |

## AWS permissions

The `aws-iam-role` credential needs to manage these resources in the cluster's account and Region: ECS services and
task definitions, Elastic Load Balancing target groups and listener rules, EC2 security groups and security group
rules, EFS file systems, mount targets, and access points, IAM roles and inline role policies (and `iam:PassRole` for
them), KMS keys and aliases, Secrets Manager secrets, CloudWatch Logs log groups, and Route 53 records in the
cluster's hosted zone. It also reads subnets (`ec2:DescribeSubnets`).

The roles that the bundle creates get only these permissions:

| Role | Permissions |
|---|---|
| Task execution role, one for each automation | `logs:CreateLogStream` and `logs:PutLogEvents` on its log group, `secretsmanager:GetSecretValue` on its secret, and `kms:Decrypt` on the instance's key. When **Image > Repository** is an ECR repository, also `ecr:GetAuthorizationToken`, and pull on that repository. |
| Task role of drift, freeze-guard, and statuspage | `elasticfilesystem:ClientMount` and `elasticfilesystem:ClientWrite` through its access point, and for statuspage also through the site access point. |
| Task role of audit-archiver | `s3:PutObject` on `<bucket>/<prefix>/*`, and `s3:PutObjectRetention` with `s3.object_lock`. |

audit-archiver uses the task role when its config sets `s3.bucket` and no `s3.endpoint`, and the
`AUDIT_ARCHIVER_AWS_ACCESS_KEY_ID` secret is not set. With the secrets, it uses the keys. If the bucket uses SSE-KMS
with a customer managed key, allow the task role `kms:GenerateDataKey` in that key's policy.

## Secrets

Each secret is set on the instance. Set only the secrets of the automations that you turn on.

| Secret | Automation | Variable |
|---|---|---|
| `SLACK_BOT_TOKEN` | bundle-scanner, drift, freeze-guard, proposal-chatops, release-notes, rollback-on-failure | `SLACK_BOT_TOKEN` |
| `SLACK_SIGNING_SECRET` | proposal-chatops | `SLACK_SIGNING_SECRET` |
| `SLACK_WEBHOOK_URL` | bundle-scanner, drift, freeze-guard, release-notes, rollback-on-failure | `SLACK_WEBHOOK_URL` |
| `AUDIT_ARCHIVER_AWS_ACCESS_KEY_ID`, `AUDIT_ARCHIVER_AWS_SECRET_ACCESS_KEY` | audit-archiver | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` |
| `AUDIT_ARCHIVER_SPLUNK_HEC_TOKEN` | audit-archiver | `SPLUNK_HEC_TOKEN` |
| `AUDIT_ARCHIVER_DD_API_KEY` | audit-archiver | `DD_API_KEY` |
| `DRIFT_RUN_TOKEN` | drift | `DRIFT_RUN_TOKEN` |
| `FREEZE_GUARD_ADMIN_TOKEN` | freeze-guard | `FREEZE_ADMIN_TOKEN` |
| `GITHUB_DEPLOYMENTS_TOKEN` | github-deployments | `GITHUB_TOKEN` |
| `RELEASE_NOTES_TEAMS_WEBHOOK_URL` | release-notes | `TEAMS_WEBHOOK_URL` |
| `ROUTER_ENV` | router | Each key of the JSON object |
| `OTEL_EXPORTER_OTLP_HEADERS` | every automation | `OTEL_EXPORTER_OTLP_HEADERS` |

## Tracing

Set **Tracing > OTLP endpoint** to send the traces of every automation to an OpenTelemetry collector over
OTLP/HTTP. Each automation gets it as `OTEL_EXPORTER_OTLP_ENDPOINT`, and the `OTEL_EXPORTER_OTLP_HEADERS` secret as
`OTEL_EXPORTER_OTLP_HEADERS`. See [Tracing](../README.md#tracing).

## Event destinations

Each destination is named `<instance>-<automation>`. The bundle writes the condition from the configuration where
the automation's README says to:

- sandbox-on-join: from the `group` of each sandbox rule.
- statuspage: from the environment `id`s of the configuration.
- github-deployments: from `github.repo_attribute`, default `github_repo`.
- rollback-on-failure: from `attribute.key` and `attribute.value`, default `autorollback` = `true`. The destination
  sends failed deployments and completed rollbacks.

autodeploy-downstream's destination sends every completed `PROVISION`; the automation applies the `upstream`
attributes of its rules itself.

The router destination sends the event types of every route in its configuration, with no attributes and no
condition. Each route filters with its own `match` and `when`.

**Event attributes** on an automation adds an attribute filter to its destination, such as
`md-environment` = `prod`. audit-archiver, freeze-guard, github-deployments, router, and sandbox-on-join have none,
because their events can have no attributes. release-notes always filters on `md-repo-artifact-type` = `bundle`.

## Limits

- Each automation runs one task. A deployment stops the old task before it starts the new one, so the automation is
  down for about a minute, and Massdriver sends the events of that time again. The automations keep their queues and
  repeated-event ids in memory, and a restart loses them.
- Tasks run on ARM64. An image that you build from the `-scripts` image must have an ARM64 variant.
- router runs the `<tag>-scripts` image when a route has an `exec` step or **Scripts** has a file. That image has
  `sh`, `jq`, `curl`, and `python3`. For other tools, build an image from it, push it with the same `-scripts`
  tag to your own repository, and set **Image > Repository**, which every automation uses.
- A task definition holds at most 64 KiB, which includes the configuration files and the router **Scripts**.
- Task sizes are fixed: 0.25 vCPU and 512 MiB, and for bundle-scanner 1 vCPU and 2 GiB.
- The bundle does not change the load balancer's security group. If it limits outbound traffic, add a rule to the
  tasks' security group on port 8080.
