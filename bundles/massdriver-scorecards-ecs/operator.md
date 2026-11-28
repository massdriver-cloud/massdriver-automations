---
templating: mustache
---
# Scorecards

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Writes:** `md/v1/scorecard/<scorecard>` on each scored instance, its environment, and its project, and the facts `md/v1/delivery`, `md/v1/reliability`, `md/v1/currency`, `md/v1/pending`, `md/v1/parity`, and `md/v1/ownership`

## A score or a fact is missing or old

Scores and facts have no TTL. They stay until the automation writes them again, so an old value means the automation stopped computing that instance.

- **Every value is old:** the automation is down. See **Read the logs** and **Check that it answers**.
- **One instance has no score:** no scorecard's `match` fits the instance's effective attributes. Only matched instances get facts and scores.
- **One fact is missing and the log says `fact skipped`:** a read for that fact failed. The line names the fact and the error. The other facts and the scores are still written. See **Errors from Massdriver**.
- **`md/v1/currency` is missing:** the instance was never deployed, or its bundle has no stable version.
- **A check is `unknown`:** its stash key is not set, has expired, or is older than its `max_age`, or its `pass` expression failed. The check's `message` says which.
- **The log says `stash entry write failed`:** the server refused the write. See **Errors from Massdriver**.

## Read what it wrote

```bash
mass alpha stash get <instance>.md/v1/scorecard/<scorecard>
mass alpha stash get <instance>.md/v1/delivery
```

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-scorecards')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Each job logs `job failed` with the job, such as `instance/billing-prod-app`, when it cannot finish. Each write logs `stash entry set`.

## Errors from Massdriver

- **`forbidden` on a write:** the service account needs `stash:write` on the scored instances, their environments, and their projects.
- **`forbidden` or `NOT_FOUND` from ownership:** reading group policies needs `organization:manageGroups`.
- **`401` or `unauthorized`:** the service account's key is gone. Give the **service_account** resource a new key and deploy.
- **A key is rejected as not well-known:** the server does not have the scorecard keys yet. Upgrade Massdriver.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with a config error:** an unknown key, a scorecard name that is not lowercase letters, digits, `_`, and `-`, a check with both or neither of `stash` and `query`, or a `pass` that is not jq stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.

## Deploy fails: Set Ingress > Host

The cluster has no DNS zone, so there is no default host. Set **Host** to a name the cluster's HTTPS listener has a certificate for, or connect a DNS zone to the cluster.

## Check that it answers

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}scorecards.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up.
