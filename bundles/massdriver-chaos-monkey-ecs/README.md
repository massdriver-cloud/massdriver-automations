# massdriver-chaos-monkey-ecs

Runs [chaos-monkey](../../automations/chaos-monkey) on Amazon ECS with Fargate. Picks matching instances at random on a schedule and sends them deployments of presets, merged presets, mutated params, and gibberish, then posts how each one went. Set the `CHAOS_MONKEY_RUN_TOKEN` secret to start runs with `POST https://<host>/chaos-monkey/run`.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run chaos-monkey`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-chaos-monkey](../massdriver-chaos-monkey). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token of the Slack app. Posts go to the channels in the settings. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, for a workspace without the Slack app. Every post goes to its one channel. Ignored when the bot token is set. |
| `CHAOS_MONKEY_RUN_TOKEN` | Callers of `POST /chaos-monkey/run` send it as `Authorization: Bearer <token>`. |
