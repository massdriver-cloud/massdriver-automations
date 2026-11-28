---
templating: mustache
---
# Bundle Scanner

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Events in:** https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}bundle-scanner.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/hooks/massdriver, from the event destination `{{params.md_metadata.name_prefix}}`
- **Scanners:** {{#params.config.scanners}}`{{.}}` {{/params.config.scanners}}{{^params.config.scanners}}`trivy` `checkov`{{/params.config.scanners}}
- **A version fails at:** {{#params.config.fail_severity}}`{{params.config.fail_severity}}`{{/params.config.fail_severity}}{{^params.config.fail_severity}}`HIGH`{{/params.config.fail_severity}}
{{#params.config.result_attribute}}
- **Result attribute:** `{{params.config.result_attribute}}` on the bundle's repo
{{/params.config.result_attribute}}
- **Posts to:** {{#params.config.slack.channel}}`#{{params.config.slack.channel}}`{{/params.config.slack.channel}}{{^params.config.slack.channel}}the logs only{{/params.config.slack.channel}}

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-bundle-scanner')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Lines from the `files` stream come from the helper that writes the settings to disk before the automation starts.

## A version wasn't scanned

- **Busy:** scans run one at a time from a queue of 100. A full queue makes Massdriver send the event again later. A restart loses the queue.
- **Already scanned:** each version is scanned once while the task runs.
- **Too big:** a bundle over 512 MiB isn't scanned.
- **Pull or scanner failed:** the error is posted to Slack and logged. Massdriver doesn't send the event again; publish a new version to scan.

## Checkov findings never fail a version

Checkov reports no severity without a Prisma Cloud key, so its findings count as `unrated`. Only Trivy findings at or above the failing severity fail a version.

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
dig +short {{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}bundle-scanner.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}bundle-scanner.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`dig` returns nothing:** the DNS record is made only when **Create a DNS record** is on and the host is in the cluster's zone{{#dependencies.ecs_cluster.zone}} (`{{dependencies.ecs_cluster.zone.domain}}`){{/dependencies.ecs_cluster.zone}}. Otherwise point the name at the cluster's load balancer yourself.
- **TLS error:** the cluster's HTTPS listener has no certificate for the host. A host outside the cluster's zone needs its own certificate on the listener.
- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`200`:** the automation is up. Look for the event in the logs.

## Nothing posts to Slack

- Set the **Slack bot token** secret (`SLACK_BOT_TOKEN`) and a channel, or the **Slack webhook URL** secret (`SLACK_WEBHOOK_URL`). The bot token wins when both are set. Deploy after you change a secret; the task restarts with it.
- With the bot token, invite the bot to the channel: `/invite @<bot name>`.
- Slack's error is in the logs: `not_in_channel` (invite the bot), `channel_not_found` (check the channel name), `invalid_auth` (the token was revoked).
