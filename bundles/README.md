# Bundles

One Massdriver bundle for each automation and platform:

| Automation | Kubernetes | Amazon ECS |
|---|---|---|
| [audit-archiver](../automations/audit-archiver) | [massdriver-audit-archiver](massdriver-audit-archiver) | [massdriver-audit-archiver-ecs](massdriver-audit-archiver-ecs) |
| [autodeploy-downstream](../automations/autodeploy-downstream) | [massdriver-autodeploy-downstream](massdriver-autodeploy-downstream) | [massdriver-autodeploy-downstream-ecs](massdriver-autodeploy-downstream-ecs) |
| [bundle-scanner](../automations/bundle-scanner) | [massdriver-bundle-scanner](massdriver-bundle-scanner) | [massdriver-bundle-scanner-ecs](massdriver-bundle-scanner-ecs) |
| [chaos-monkey](../automations/chaos-monkey) | [massdriver-chaos-monkey](massdriver-chaos-monkey) | [massdriver-chaos-monkey-ecs](massdriver-chaos-monkey-ecs) |
| [cost-explorer](../automations/cost-explorer) | [massdriver-cost-explorer](massdriver-cost-explorer) | [massdriver-cost-explorer-ecs](massdriver-cost-explorer-ecs) |
| [drift](../automations/drift) | [massdriver-drift](massdriver-drift) | [massdriver-drift-ecs](massdriver-drift-ecs) |
| [freeze-guard](../automations/freeze-guard) | [massdriver-freeze-guard](massdriver-freeze-guard) | [massdriver-freeze-guard-ecs](massdriver-freeze-guard-ecs) |
| [github-deployments](../automations/github-deployments) | [massdriver-github-deployments](massdriver-github-deployments) | [massdriver-github-deployments-ecs](massdriver-github-deployments-ecs) |
| [pr-sync](../automations/pr-sync) | [massdriver-pr-sync](massdriver-pr-sync) | [massdriver-pr-sync-ecs](massdriver-pr-sync-ecs) |
| [proposal-chatops](../automations/proposal-chatops) | [massdriver-proposal-chatops](massdriver-proposal-chatops) | [massdriver-proposal-chatops-ecs](massdriver-proposal-chatops-ecs) |
| [release-notes](../automations/release-notes) | [massdriver-release-notes](massdriver-release-notes) | [massdriver-release-notes-ecs](massdriver-release-notes-ecs) |
| [rollback-on-failure](../automations/rollback-on-failure) | [massdriver-rollback-on-failure](massdriver-rollback-on-failure) | [massdriver-rollback-on-failure-ecs](massdriver-rollback-on-failure-ecs) |
| [router](../automations/router) | [massdriver-router](massdriver-router) | [massdriver-router-ecs](massdriver-router-ecs) |
| [sandbox-on-join](../automations/sandbox-on-join) | [massdriver-sandbox-on-join](massdriver-sandbox-on-join) | [massdriver-sandbox-on-join-ecs](massdriver-sandbox-on-join-ecs) |
| [stale-proposals](../automations/stale-proposals) | [massdriver-stale-proposals](massdriver-stale-proposals) | [massdriver-stale-proposals-ecs](massdriver-stale-proposals-ecs) |
| [stash-sync](../automations/stash-sync) | [massdriver-stash-sync](massdriver-stash-sync) | [massdriver-stash-sync-ecs](massdriver-stash-sync-ecs) |
| [statuspage](../automations/statuspage) | [massdriver-statuspage](massdriver-statuspage) | [massdriver-statuspage-ecs](massdriver-statuspage-ecs) |
| [upgrade-train](../automations/upgrade-train) | [massdriver-upgrade-train](massdriver-upgrade-train) | [massdriver-upgrade-train-ecs](massdriver-upgrade-train-ecs) |

Every bundle:

- runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run <automation>` at **Ingress >
  Host**
- creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the event types,
  attributes, and condition that the automation uses, and gives the automation its signing secret
- writes **Settings** as the automation's config file. The settings have the keys of the config file. A setting
  left empty uses the automation's default.
- calls the Massdriver API with the `massdriver-service-account` dependency, which needs
  `organization:manageEventDestinations` and the permissions in the automation's README

massdriver-statuspage and massdriver-cost-explorer also produce a `website` resource with the URL to open.

Publish the resource types that a bundle uses before you publish the bundle. See
[Deploy](../README.md#deploy) for the list: `massdriver-service-account` and `massdriver-kubernetes-cluster` are
official, `website` and `aws-ecs-cluster` are examples in this repo, and `aws-iam-role` is the AWS credential type.

An attribute value with commas, such as `api, web`, matches any of its values, where the config file accepts a
list: the attributes of autodeploy-downstream rules, chaos-monkey's match, drift checks, freeze-guard windows, upgrade-train stages, and stale-proposals' match, exclude, and mentions.

## Kubernetes

Depends on a [`massdriver-kubernetes-cluster`](https://github.com/massdriver-cloud/massdriver-resource-types/tree/main/massdriver-kubernetes-cluster).
The bundle deploys into the namespace of that resource, or `default`. It does not create the namespace. It creates:

- a Deployment with one replica, a Secret with the environment variables, and a ConfigMap with the config file
- a Service, and an Ingress for the host that sends every path to the automation
- for cost-explorer, drift, freeze-guard, statuspage, and upgrade-train, a PersistentVolumeClaim for `STATE_DIR`
- for statuspage with an output directory, a second claim `<instance>-site`, mounted at that directory
- for router, a ConfigMap with its **Scripts**, mounted as executables in `/scripts`

stash-sync reads CloudWatch with the `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` secrets on Kubernetes.

The cluster needs an ingress controller, a DNS record for the host, and TLS: Massdriver and Slack send requests
only to `https://` URLs. Set a cert-manager ClusterIssuer, set a TLS secret, or end TLS in front of the ingress
controller.

Requests and limits: 25m CPU and 64Mi memory, up to 500m and 256Mi. bundle-scanner gets 250m and 512Mi, up to 2
CPUs and 2Gi.

## Amazon ECS

Depends on an `aws-iam-role` and an `aws-ecs-cluster`. The `aws-iam-role` can be the AWS credential type from
[massdriver-catalog](https://github.com/massdriver-cloud/massdriver-catalog/tree/main/platforms/aws), with `arn`
and an optional `external_id`, or a type with `arn` and `token_level`, where the role trusts the deployment's
Massdriver identity token of that level. The `aws-ecs-cluster` type is an example in
[examples/resource-types/aws-ecs-cluster](../examples/resource-types/aws-ecs-cluster). The bundle creates:

- an ECS service with one Fargate task (ARM64). An init container writes the config file and router's **Scripts**
  to task volumes, because the image has no shell.
- a Secrets Manager secret with the secret environment variables, a CloudWatch log group, and a KMS key for both
- a task execution role that can read only that secret and write only that log group
- a target group with a health check on `/healthz`, and a rule for the host on the cluster's HTTPS listener
- a CNAME record for the host, when it is in the cluster's hosted zone and **Create a DNS record** is on
- for cost-explorer, drift, freeze-guard, statuspage, and upgrade-train, an EFS file system and an access point for `STATE_DIR`,
  with a task role that can mount only that access point
- for audit-archiver with an S3 bucket and no AWS key secrets, a task role that can write to the bucket
- for stash-sync with CloudWatch rules or presets and no AWS key secrets, a task role that can call
  `cloudwatch:GetMetricData` and `cloudwatch:DescribeAlarms`

**Ingress > Host** defaults to `<automation>.<domain of the cluster's hosted zone>`. The listener needs a
certificate for it.

Task size: 0.25 vCPU and 512 MiB. bundle-scanner gets 1 vCPU and 2 GiB.

## Change the bundles

The bundles are generated. Edit the sources, then run the script and `mass bundle build` in each bundle:

- `bundles/_shared/kubernetes/` and `bundles/_shared/ecs/`: the platform files
- `bundles/_shared/automations/<automation>.tf`: what is particular to an automation, such as its event types and
  the condition of its destination
- `scripts/bundles.py`: the settings forms, secrets, and READMEs

```bash
python3 scripts/bundles.py
for b in bundles/massdriver-*/; do (cd "$b" && mass bundle build); done
```

## Limits

- Each automation runs one replica and restarts on every change of its settings. It keeps its queues and repeated
  event IDs in memory, and a restart loses them.
- router runs the `<tag>-scripts` image when a route has an `exec` step or **Scripts** has a file. That image has
  `sh`, `jq`, `curl`, and `python3`.
