# massdriver-upgrade-train-ecs

Runs [upgrade-train](../../automations/upgrade-train) on Amazon ECS with Fargate. Moves each new bundle version through stages of instances, such as dev, staging, and prod, with a soak time and an alarm check between stages. Set the `UPGRADE_TRAIN_TOKEN` secret to start, resume, and stop runs with `POST https://<host>/upgrade-train/run` and `/halt`.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run upgrade-train`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-upgrade-train](../massdriver-upgrade-train). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, for a workspace without the Slack app. Every post goes to its one channel. Ignored when the bot token is set. |
| `UPGRADE_TRAIN_TOKEN` | Callers of `POST /upgrade-train/run` and `POST /upgrade-train/halt` send it as `Authorization: Bearer <token>`. |
