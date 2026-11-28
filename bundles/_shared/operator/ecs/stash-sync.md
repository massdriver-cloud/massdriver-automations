---
templating: mustache
---
# Stash Sync

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **AWS access:** the task role{{#dependencies.ecs_cluster.region}} in {{dependencies.ecs_cluster.region}}{{/dependencies.ecs_cluster.region}}, unless the **AWS access key ID** and **AWS secret access key** secrets are set
- **Settings:**

```yaml
{{{params.config}}}
```

## A widget went away

Every entry expires after three intervals without a write, so a widget that disappears means the automation stopped writing it.

- **Every widget is gone:** the automation is down. See **The task won't start or keeps restarting**.
- **One widget is gone:** its query failed or found no data, and the rule has no `default`. The log names the rule and the error.

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-stash-sync')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

## A widget shows the wrong value

- **Wrong instance or environment:** the rule's `target` names the identifier, such as `billing-dev-app`, not the AWS name.
- **A dimension is wrong:** CloudWatch returns no data, not an error. Compare the rule's dimensions with the metric in the CloudWatch console. `load_balancer` and `target_group` are the end of each ARN, such as `app/name/0123456789abcdef`.
- **The value is a few minutes old:** CloudWatch metrics lag. The automation uses the latest data point in the last three periods.
- **The log says a value does not match the schema:** the value doesn't fit the widget, for example `md/v1/health` text longer than 64 characters. Fix the rule and deploy.

## Errors from CloudWatch

- **`AccessDenied` for `GetMetricData` or `DescribeAlarms`:** the task role gets CloudWatch read only when the settings have a CloudWatch rule or an `aws-` preset in `sync` mode. Deploy after adding the first one. With the access key secrets set, those keys need the permissions instead.
- **The AWS bill grew:** CloudWatch bills `GetMetricData` for each metric, each cycle. Raise `interval` or drop rules.

## Errors from Massdriver

- **`forbidden` on a write:** the service account needs `instance:configure` on the instance, or `environment:update` on the environment, that the rule writes to.
- **`401` or `unauthorized`:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with a config error:** an unknown key, a bad preset setting, or a rule with two sources stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.

## Deploy fails: Set Ingress > Host

The cluster has no DNS zone, so there is no default host. Set **Host** to a name the cluster's HTTPS listener has a certificate for, or connect a DNS zone to the cluster.

## Check that it answers

The automation acts on no event; the event destination `{{params.md_metadata.name_prefix}}` sends nothing and is there for its signing secret.

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}stash-sync.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up.
