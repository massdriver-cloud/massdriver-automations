# proposal-chatops

When somebody proposes a deployment, post it to Slack with **Approve** and **Reject** buttons. The message shows
the instance, the action, the bundle version, who proposed it, and the deployment message.

A click calls `approveDeployment` or `rejectDeployment` as the service account. The message then shows the
decision and the Slack user who clicked, for example `Approved by @sam in Slack`. If the call fails, for example
because the deployment is no longer `PROPOSED`, only the person who clicked sees the error.

A proposal that is approved or rejected in Massdriver, the CLI, or the API also updates its Slack message, for
example `Rejected by Sam Lee in Massdriver`, and the buttons go away.

## Slack app

Use the [Slack app](../../README.md#slack) of the repository. Set its interactivity request URL to
`https://<host>/slack/interactions`. proposal-chatops needs the bot token: a webhook cannot update a message or
receive a click.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `SLACK_BOT_TOKEN` | Required. The bot token of the Slack app. |
| `SLACK_SIGNING_SECRET` | Required. The signing secret of the Slack app. Clicks without a valid signature get `401`. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key stops the automation at start.

| Key | Default | |
|---|---|---|
| `slack.channel` | | Required. The channel name or ID for proposals. |
| `slack.channel_attribute` | | An attribute key. When the instance has this attribute, its value names the channel instead of `slack.channel`. |
| `slack.allowed_users` | | Slack user IDs that can click the buttons. Empty allows everybody in the channel. |

The service account needs `instance:deploy` on the instances. Massdriver gates approval and rejection with that
permission.

If the environment has separation of duty turned on, a proposal that the service account made itself cannot be
approved from Slack.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name proposal-chatops \
  --url https://chatops.example.com/hooks/massdriver \
  --event-type deployment.proposed \
  --event-type deployment.approved \
  --event-type deployment.rejected
```

The automation acts on every event of these types, so the destination needs no condition. To post proposals of
one environment only, add `--attribute md-environment=prod`.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e SLACK_BOT_TOKEN=xoxb-... \
  -e SLACK_SIGNING_SECRET=... \
  massdrivercloud/massdriver-automations:latest run proposal-chatops
```

Massdriver and Slack must both reach the service: `/hooks/massdriver` and `/slack/interactions`.

## Limits

- Massdriver records the service account, not the Slack user, as the approver. The V2 API has no field for a
  reviewer or a comment on `approveDeployment` or `rejectDeployment`. The Slack user is in the Slack message and
  in the log line `deployment decided`.
- The Slack message of each proposal is remembered in memory for 30 days. After a restart, the buttons on
  earlier messages still work, but an approval or rejection made in Massdriver does not update an earlier message.
  Run one replica.
- Massdriver sends events in any order. When the approval or rejection arrives before the proposal, the message is
  posted already decided, without buttons.
- When Slack refuses a message, the automation responds `500`, and Massdriver sends the event again.
- Anybody who can click in the channel can approve, unless `slack.allowed_users` is set.
