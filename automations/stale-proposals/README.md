# stale-proposals

On a schedule, reads every proposed deployment in the organization and posts one digest to Slack of those that have
waited longer than `remind_after` for approval. Each line links to the proposal in the instance's history, and
says how long it has waited, who proposed it, and the first line of its message. Rules add Slack mentions to the
proposals they match, so the people who can approve them see them.

With `reject_after`, the automation also rejects the proposals that have waited that long, and lists them under
their own heading. A proposal that cannot be rejected, such as one approved since the run started, is listed with
the reason. With `dry_run`, it lists what it would reject and rejects nothing.

```text
2 proposed deployments have waited more than 1d for approval.

*api / prod*
- api-prod-db waiting 2d 2h, proposed by sam@acme.com: "Bump storage" @platform-oncall
- api-prod-cache waiting 1d 6h, proposed by lee@acme.com

*Rejected: waited more than 14d*
- web-dev-cdn waited 15d 3h, proposed by drift
```

A run with no stale proposals posts nothing.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `STALE_PROPOSALS_RUN_TOKEN` | Serves `POST /stale-proposals/run`. Callers send it as `Authorization: Bearer <token>`. |
| `SLACK_BOT_TOKEN` | The bot token of the [Slack app](../../README.md#slack). Posts to the channel in the config. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, when there is no Slack app. Posts every message to its one channel. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

| Key | Default | |
|---|---|---|
| `schedule` | | Standard cron, or `@daily`. Empty runs only from `POST /stale-proposals/run`. |
| `timezone` | `UTC` | An IANA zone for the schedule. |
| `remind_after` | `24h` | How long a proposal waits before it is in the digest. A Go duration, such as `36h`, or days, such as `2d`. |
| `reject_after` | | Reject proposals that have waited this long. Not shorter than `remind_after`. Empty rejects none. |
| `dry_run` | `false` | List what `reject_after` would reject, and reject nothing. |
| `group_by` | `environment` | `environment`: a heading for each project and environment. `project`: a heading for each project. |
| `match` | | Effective attributes that a proposal's instance must have. A list matches any of its values. Empty matches every proposal. |
| `exclude` | | Skip the proposals whose instance has every one of these attributes. |
| `mentions[].match` | | Effective attributes of the proposals that get the mentions. Empty matches every proposal. |
| `mentions[].mentions` | | Slack mentions, such as `<@U123>`, `<!subteam^S123>`, or `<!here>`. |
| `slack.channel` | | The channel for the digest, with `SLACK_BOT_TOKEN`. Without a channel or a webhook, the digest goes to the logs only, without its mentions. |
| `massdriver.app_url` | `https://app.massdriver.cloud` | The Massdriver UI, for the links in the digest. |

### Run now

```bash
curl -X POST -H "Authorization: Bearer $STALE_PROPOSALS_RUN_TOKEN" https://proposals.example.com/stale-proposals/run
```

The service account needs these permissions:

- `project:view` on the projects to watch, to list their deployments
- `instance:deploy` on their instances, for `reject_after`. Massdriver gates rejection with it, as it does approval.

## Event destination

The automation reads proposals from the API, and acts on no event. Every automation receives events from a
destination, so create one that sends nothing:

```bash
mass event destination create \
  --name stale-proposals \
  --url https://proposals.example.com/hooks/massdriver \
  --event-type deployment.proposed \
  --condition 'false'
```

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e SLACK_BOT_TOKEN=xoxb-... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run stale-proposals
```

## Limits

- The digest is one Slack message. Slack cuts a message at 40,000 characters, so with hundreds of stale proposals
  the end of the digest is lost. Narrow `match`, or lower `reject_after`.
- Runs do not overlap. A run that starts while another is going waits for it.
- A proposal waits from when it was created. Editing the instance does not reset its age.
