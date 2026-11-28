---
templating: mustache
---
# Upgrade Train

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Events in:** https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}upgrade-train.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/hooks/massdriver, from the event destination `{{params.md_metadata.name_prefix}}`
- **Posts go to:** {{#params.config.slack.channel}}`#{{params.config.slack.channel}}`{{/params.config.slack.channel}}{{^params.config.slack.channel}}the logs only{{/params.config.slack.channel}}
- **Trains:**
{{#params.config.trains}}
  - {{name}}: {{#bundles}}`{{.}}` {{/bundles}}{{#include_development}}(dev releases too){{/include_development}}
{{#stages}}
    - {{name}}: {{#mode}}`{{mode}}`{{/mode}}{{^mode}}`propose`{{/mode}}, soak {{#soak}}{{soak}}{{/soak}}{{^soak}}1h{{/soak}}
{{/stages}}
{{/params.config.trains}}

## Stop a train now

Needs the **Run token** secret:

```bash
curl -X POST -H "Authorization: Bearer $UPGRADE_TRAIN_TOKEN" "https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}upgrade-train.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/upgrade-train/halt?train=<train name>"
```

Add `&bundle=<bundle>` to stop one bundle only. Without the token, remove the train from **Settings** and deploy; deployments it already created keep going.

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-upgrade-train')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Every deployment the train creates has the message `upgrade-train: <train> <bundle>@<version> stage <stage>`. Search the instance's history for it.

## A train stopped

The post and the log say why. Fix the cause, then resume at the stage where it stopped:

```bash
curl -X POST -H "Authorization: Bearer $UPGRADE_TRAIN_TOKEN" \
  "https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}upgrade-train.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/upgrade-train/run?train=<train name>&bundle=<bundle>&version=<version>"
```

- **A deployment failed, was aborted, or was rejected:** open it in Massdriver for the reason.
- **An alarm fired during the soak:** look at the instance's alarms. The run waits until you resume it.
- **An instance left the version during the soak:** someone deployed another version. Resume when it is back, or let the next release start a new run.

A newer version of the bundle replaces the stopped run, so a fix released as a new version needs no resume.

## A new version didn't start a run

- The bundle isn't in the train's **Bundles**. Names are the repository names, such as `aws-rds-mysql`.
- It is a development release and **Include development versions** is off.
- The instance isn't `PROVISIONED`, or its attributes don't match the stage.
- The instance's version constraint doesn't resolve to the new version, for example it is pinned to an exact version. The post names these instances. The train never changes a version constraint.
- The instance had a deployment pending or running for an hour. The stage leaves it out and says so.

## A prod stage is waiting

Stages in `propose` mode create a proposal for each instance. A person approves each one in Massdriver; the soak starts when all of them complete. A proposal nobody approves stops nothing and waits.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with a config error:** an unknown key or a bad value in the settings stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.
- **`401` or `unauthorized` from Massdriver:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

Runs in progress are kept on EFS, so a restart picks them up again.

## Deploy fails: Set Ingress > Host

The cluster has no DNS zone, so there is no default host. Set **Host** to a name the cluster's HTTPS listener has a certificate for, or connect a DNS zone to the cluster.

## Events don't arrive

Massdriver sends events to the event destination `{{params.md_metadata.name_prefix}}` (**Settings → Event Destinations**). It sends bundle releases and the train's own deployments only. Massdriver pauses a destination when sends fail for a day; the next deploy of this instance resumes it.

Check that the automation answers:

```bash
dig +short {{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}upgrade-train.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}upgrade-train.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`dig` returns nothing:** the DNS record is made only when **Create a DNS record** is on and the host is in the cluster's zone{{#dependencies.ecs_cluster.zone}} (`{{dependencies.ecs_cluster.zone.domain}}`){{/dependencies.ecs_cluster.zone}}. Otherwise point the name at the cluster's load balancer yourself.
- **TLS error:** the cluster's HTTPS listener has no certificate for the host.
- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up. Look for the event in the logs.

## Nothing posts to Slack

- Set the **Slack bot token** secret (`SLACK_BOT_TOKEN`) and a channel, or the **Slack webhook URL** secret (`SLACK_WEBHOOK_URL`). The bot token wins when both are set. Deploy after you change a secret; the task restarts with it.
- With the bot token, invite the bot to the channel: `/invite @<bot name>`.
- Slack's error is in the logs: `not_in_channel` (invite the bot), `channel_not_found` (check the channel name), `invalid_auth` (the token was revoked).
