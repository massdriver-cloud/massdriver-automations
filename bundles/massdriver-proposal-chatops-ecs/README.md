# massdriver-proposal-chatops-ecs

Runs [proposal-chatops](../../automations/proposal-chatops) on Amazon ECS with Fargate. Posts each proposed deployment to Slack with Approve and Reject buttons. Needs the `SLACK_BOT_TOKEN` and `SLACK_SIGNING_SECRET` secrets. Set the Slack app's interactivity URL to `https://<host>/slack/interactions`.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run proposal-chatops`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-proposal-chatops](../massdriver-proposal-chatops). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_SIGNING_SECRET` | The signing secret of the Slack app, to check button clicks. |
