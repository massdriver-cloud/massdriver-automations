# massdriver-release-notes-ecs

Runs [release-notes](../../automations/release-notes) on Amazon ECS with Fargate. Posts each new bundle version, and the instances that can upgrade, to Slack or Microsoft Teams.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run release-notes`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-release-notes](../massdriver-release-notes). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, for a workspace without the Slack app. Every post goes to its one channel. Ignored when the bot token is set. |
| `TEAMS_WEBHOOK_URL` | A Microsoft Teams Workflows webhook, for `teams.enabled`. |
