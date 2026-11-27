# massdriver-stale-proposals

Runs [stale-proposals](../../automations/stale-proposals) on a Kubernetes cluster. Posts a digest of the proposed deployments that have waited too long for approval, with Slack mentions for the people who can approve them, and can reject the oldest. Set the `STALE_PROPOSALS_RUN_TOKEN` secret to start runs with `POST https://<host>/stale-proposals/run`.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run stale-proposals`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-stale-proposals-ecs](../massdriver-stale-proposals-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, for a workspace without the Slack app. Every post goes to its one channel. Ignored when the bot token is set. |
| `STALE_PROPOSALS_RUN_TOKEN` | Callers of `POST /stale-proposals/run` send it as `Authorization: Bearer <token>`. |
