# massdriver-bundle-scanner-ecs

Runs [bundle-scanner](../../automations/bundle-scanner) on Amazon ECS with Fargate. Scans each new bundle version with Trivy and Checkov, and posts the results.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run bundle-scanner`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-bundle-scanner](../massdriver-bundle-scanner). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, for a workspace without the Slack app. Every post goes to its one channel. Ignored when the bot token is set. |
