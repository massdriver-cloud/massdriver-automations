---
templating: mustache
---
# Scorecards

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Settings:**

```yaml
{{{params.config}}}
```

## A scorecard is missing or old

The automation writes `md/v1/scorecard/<name>` on each deployed instance that a scorecard matches, and a roll-up on its environment and project. It writes again on each deployment, alarm, instance change, new bundle version, and change to a key a check reads, and on every interval.

- **Every scorecard is old:** the automation is down. See **Read the logs** and **Check that it answers**.
- **One instance has no scorecard:** it was never deployed, it was decommissioned, or its attributes don't match the scorecard's `match`.
- **The log says `stash entry write failed` with `not a well-known key`:** the server's widget catalog does not have the key yet. Set `results.prefix` to `results/v1` in the settings and deploy.
- **The log says `fact skipped`:** a read failed, so that fact was left out this time. The next event or interval tries again.

## A check is unknown

A check is unknown when the key it reads is missing, expired, or older than its `max_age`. The check's `message` says which. Look at the key:

```bash
mass alpha stash get <instance>.md/v1/drift
```

- **The key is missing:** the automation that writes it doesn't cover this instance. For example, drift writes `md/v1/drift` and log-results writes `md/v1/compliance/src`.
- **The key is old:** the automation that writes it stopped, or the check's `max_age` is shorter than that automation's schedule.

## Read what it wrote

```bash
mass alpha stash get <instance>.md/v1/scorecard/<name>
mass alpha stash get <environment>.md/v1/scorecard/<name>
mass alpha stash get <instance>.md/v1/delivery
```

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-scorecards')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

## Errors from Massdriver

- **`does not match md/v1/...`:** a value doesn't fit its widget. This is a bug in the automation; the log line has the key.
- **`forbidden` on a write:** the service account needs `stash:write` on the instances, environments, and projects it scores.
- **`job failed` for `ownership`:** the service account needs `organization:manageGroups` to read what each group may do.
- **`401` or `unauthorized`:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with a config error:** an unknown key, a `pass` that isn't jq, or two checks with one `id` stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.

## Check that it answers

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}scorecards.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up.
