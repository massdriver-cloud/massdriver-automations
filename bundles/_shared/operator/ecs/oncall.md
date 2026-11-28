---
templating: mustache
---
# On Call

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Settings:**

```yaml
{{{params.config}}}
```

## An environment's on-call widget is empty or old

The automation writes `md/v1/oncall` on each environment in its routes every interval. Each write lasts three intervals, so the widget empties soon after the automation stops.

- **Every environment is empty:** the automation is down. See **Read the logs** and **Check that it answers**.
- **One environment is empty:** it isn't in any route, or the server refused the write. The log line `oncall write failed` names the environment and the server's reason.
- **A PagerDuty or incident.io route's list stops changing:** `oncall query failed` means `PAGERDUTY_TOKEN` or `INCIDENT_IO_API_KEY` is missing, expired, or the ID in the route doesn't exist.

## Read what it wrote

```bash
mass alpha stash get <environment>.md/v1/oncall
```

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-oncall')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

## Errors from Massdriver

- **`does not match md/v1/oncall`:** an entry in a `static` list doesn't fit the widget, such as a name longer than 64 characters or more than 25 people. Fix the setting and deploy.
- **`forbidden` on a write:** the service account needs `stash:write` on each environment in the routes.
- **`401` or `unauthorized`:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with a config error:** an unknown key, an environment in two routes, or a route with no source stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.

## Check that it answers

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}oncall.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up.
