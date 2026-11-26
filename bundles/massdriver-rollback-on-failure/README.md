# massdriver-rollback-on-failure

Runs [rollback-on-failure](../../automations/rollback-on-failure) on a Kubernetes cluster. When a deployment of an opted-in instance fails, deploys its last completed deployment again.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run rollback-on-failure`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-rollback-on-failure-ecs](../massdriver-rollback-on-failure-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, for a workspace without the Slack app. Every post goes to its one channel. Ignored when the bot token is set. |
