# massdriver-freeze-guard

Runs [freeze-guard](../../automations/freeze-guard) on a Kubernetes cluster. During each window, adds deny policies for deployments to every custom group except the exempt groups, and removes them when it ends. Set the `FREEZE_ADMIN_TOKEN` secret to lift a window early with `POST https://<host>/freeze/lift?window=<name>`.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run freeze-guard`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-freeze-guard-ecs](../massdriver-freeze-guard-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, for a workspace without the Slack app. Every post goes to its one channel. Ignored when the bot token is set. |
| `FREEZE_ADMIN_TOKEN` | Callers of `POST /freeze/lift` send it as `Authorization: Bearer <token>`. |
