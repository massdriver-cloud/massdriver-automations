---
templating: mustache
---
# Freeze Guard

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Events in:** https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}freeze-guard.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/hooks/massdriver, from the event destination `{{params.md_metadata.name_prefix}}`
- **Time zone:** {{#params.config.timezone}}{{params.config.timezone}}{{/params.config.timezone}}{{^params.config.timezone}}UTC{{/params.config.timezone}}
- **Windows:** {{#params.config.windows}}`{{name}}` {{/params.config.windows}}{{^params.config.windows}}none{{/params.config.windows}}
- **Exempt groups:** {{#params.config.exempt_groups}}`{{.}}` {{/params.config.exempt_groups}}{{^params.config.exempt_groups}}none{{/params.config.exempt_groups}}
- **Announces in:** {{#params.config.slack.channel}}`#{{params.config.slack.channel}}`{{/params.config.slack.channel}}{{^params.config.slack.channel}}no Slack channel{{/params.config.slack.channel}}

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-freeze-guard')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Lines from the `files` stream come from the helper that writes the settings to disk before the automation starts.

## Lift a freeze in an emergency

Needs the **Admin token** secret. Anyone with it can lift a freeze.

```bash
curl -X POST -H "Authorization: Bearer $FREEZE_ADMIN_TOKEN" \
  "https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}freeze-guard.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/freeze/lift?window=<window name>"
```

- **`200`:** lifted. The deny policies are deleted. A weekly window comes back at its next start. A fixed window stays lifted until it ends.
- **`202`:** lifted, but deleting a policy failed. It retries every minute.
- **`401`:** wrong token. **`404`:** no window has that name. **`409`:** the window isn't active.

## Someone can't deploy, or can deploy when they shouldn't

During a window, each custom group that isn't exempt gets a deny policy for `instance:deploy`, `instance:decommission`, `environment:deploy` and `environment:decommission` (unless the window lists its own actions). Policies change within a minute of a window starting or ending.

- **Deny wins:** a person in an exempt group and also in a frozen group is denied. Put people who must deploy during a freeze only in exempt groups.
- **Service accounts are frozen like people.** Put a CI service account that must deploy only in an exempt group.
- **Administrators are never frozen.** Massdriver doesn't allow policies on that group.
- **Planning, proposing and editing still work.** A frozen member can't approve a proposal until the window ends.
- **A deleted policy comes back** within a minute while the window is active. To let a group deploy, make it exempt or lift the window.

## A deployment ran during a freeze

A freeze blocks new requests only. A running deployment finishes. A deployment that was already waiting (`PENDING` or `APPROVED`), and the rest of an environment deploy that had started, still run. Abort it in Massdriver to stop it.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with a config error:** an unknown key or a bad value in the settings stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.
- **`401` or `unauthorized` from Massdriver:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## Deploy fails: Set Ingress > Host

The cluster has no DNS zone, so there is no default host. Set **Host** to a name the cluster's HTTPS listener has a certificate for, or connect a DNS zone to the cluster.

## Events don't arrive

Massdriver sends events to the event destination `{{params.md_metadata.name_prefix}}` (**Settings → Event Destinations**). It only sends the event types this automation needs. Massdriver pauses a destination when sends fail for a day; the next deploy of this instance resumes it.

Check that the automation answers:

```bash
dig +short {{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}freeze-guard.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}freeze-guard.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`dig` returns nothing:** the DNS record is made only when **Create a DNS record** is on and the host is in the cluster's zone{{#dependencies.ecs_cluster.zone}} (`{{dependencies.ecs_cluster.zone.domain}}`){{/dependencies.ecs_cluster.zone}}. Otherwise point the name at the cluster's load balancer yourself.
- **TLS error:** the cluster's HTTPS listener has no certificate for the host. A host outside the cluster's zone needs its own certificate on the listener.
- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`200`:** the automation is up. Look for the event in the logs.

## Nothing posts to Slack

- Set the **Slack bot token** secret (`SLACK_BOT_TOKEN`) and a channel, or the **Slack webhook URL** secret (`SLACK_WEBHOOK_URL`). The bot token wins when both are set. Deploy after you change a secret; the task restarts with it.
- With the bot token, invite the bot to the channel: `/invite @<bot name>`.
- Slack's error is in the logs: `not_in_channel` (invite the bot), `channel_not_found` (check the channel name), `invalid_auth` (the token was revoked).
