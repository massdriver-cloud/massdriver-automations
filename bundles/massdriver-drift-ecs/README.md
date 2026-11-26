# massdriver-drift-ecs

Runs [drift](../../automations/drift) on Amazon ECS with Fargate. Plans the instances of each check on a schedule, and alerts Slack when the cloud no longer matches the last deployment. Set the `DRIFT_RUN_TOKEN` secret to start checks with `POST https://<host>/drift/run?check=<name>`.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run drift`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-drift](../massdriver-drift). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, for a workspace without the Slack app. Every post goes to its one channel. Ignored when the bot token is set. |
| `DRIFT_RUN_TOKEN` | Callers of `POST /drift/run` send it as `Authorization: Bearer <token>`. |
