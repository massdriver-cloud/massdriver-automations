---
templating: mustache
---
# Stale Proposals

- **Schedule:** {{#params.config.schedule}}`{{params.config.schedule}}` ({{#params.config.timezone}}{{params.config.timezone}}{{/params.config.timezone}}{{^params.config.timezone}}UTC{{/params.config.timezone}}){{/params.config.schedule}}{{^params.config.schedule}}only on request{{/params.config.schedule}}
- **Reminds after:** {{#params.config.remind_after}}{{params.config.remind_after}}{{/params.config.remind_after}}{{^params.config.remind_after}}24h{{/params.config.remind_after}}
- **Rejects after:** {{#params.config.reject_after}}{{params.config.reject_after}}{{#params.config.dry_run}} (dry run: lists only){{/params.config.dry_run}}{{/params.config.reject_after}}{{^params.config.reject_after}}never{{/params.config.reject_after}}
- **Digest goes to:** {{#params.config.slack.channel}}`#{{params.config.slack.channel}}`{{/params.config.slack.channel}}{{^params.config.slack.channel}}the logs only{{/params.config.slack.channel}}
- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-stale-proposals')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Without a Slack channel or webhook, the digest is in this log.

## Send the digest now

Needs the **Run token** secret:

```bash
curl -X POST -H "Authorization: Bearer $STALE_PROPOSALS_RUN_TOKEN" "https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}stale-proposals.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/stale-proposals/run"
```

`202` means it started. A run with no stale proposals posts nothing.

## It rejected a proposal it shouldn't have

Turn on **Dry run**, or clear **Reject after**, and deploy. A rejected proposal can't be brought back; propose the deployment again from the instance in Massdriver.

## A proposal is missing from the digest

- It hasn't waited longer than **Remind after** yet. Its age counts from when it was proposed.
- Its instance doesn't match **Match**, or matches every attribute in **Exclude**.
- The service account can't see the project. It needs `project:view` there.

## A proposal wasn't rejected

The digest lists it with the reason. Usually it was approved after the run started, or the service account lacks `instance:deploy` on the instance.

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

## The run endpoint doesn't answer

The automation acts on no event; the event destination `{{params.md_metadata.name_prefix}}` sends nothing and is there for its signing secret. Check the host:

```bash
dig +short {{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}stale-proposals.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}stale-proposals.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`dig` returns nothing:** the DNS record is made only when **Create a DNS record** is on and the host is in the cluster's zone{{#dependencies.ecs_cluster.zone}} (`{{dependencies.ecs_cluster.zone.domain}}`){{/dependencies.ecs_cluster.zone}}.
- **TLS error:** the cluster's HTTPS listener has no certificate for the host.
- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.

## Nothing posts to Slack

- Set the **Slack bot token** secret (`SLACK_BOT_TOKEN`) and a channel, or the **Slack webhook URL** secret (`SLACK_WEBHOOK_URL`). The bot token wins when both are set. Deploy after you change a secret; the task restarts with it.
- With the bot token, invite the bot to the channel: `/invite @<bot name>`. Mentions only work with the bot token.
- Slack's error is in the logs: `not_in_channel` (invite the bot), `channel_not_found` (check the channel name), `invalid_auth` (the token was revoked).
