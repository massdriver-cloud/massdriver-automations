---
templating: mustache
---
# Log Results

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Settings:**

```yaml
{{{params.config}}}
```

## A deployment has no step, changes, or compliance widget

The automation reads the logs of each finished `PROVISION` and `PLAN` deployment and writes `md/v1/*` entries on it.

- **No deployment gets them:** the automation is down, or its event destination is gone. See **Read the logs**.
- **One deployment has none:** its logs did not match any rule, or the server refused an entry. The log line `stash write failed` names the rule, the key, and the server's reason. `logs unavailable` means the logs never arrived.

## Check a rule on a saved log

```bash
mass deployment logs <deployment-id> > deploy.log
docker run --rm -i -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  {{params.image.repository}}:{{params.image.tag}} test log-results -action PROVISION < deploy.log
```

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-log-results')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

## Errors from Massdriver

- **`does not match md/v1/...`:** a rule's value doesn't fit the widget, such as an extra field or a message longer than 280 characters. Check the rule on a saved log and deploy.
- **`forbidden` on a write:** the service account needs `stash:write` on the instance whose deployment it writes to.
- **`401` or `unauthorized`:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with a config error:** an unknown key, a pattern that doesn't compile, or a value that isn't valid jq stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.

## Check that it answers

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}log-results.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up.
