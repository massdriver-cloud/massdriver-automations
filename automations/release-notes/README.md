# release-notes

When a new bundle version is published, post it to Slack, Microsoft Teams, or both. The message has:

- the bundle name and version
- the changelog of that version, when the publisher included one
- the instances that can upgrade to it, by project and environment, with the version each one is on
- the instances on a release channel, such as `~1` or `latest`, that Massdriver moves to the new version and
  deploys, with the version each one has deployed

An instance is listed only when it is `PROVISIONED` or `FAILED`. An instance can upgrade when its
`availableUpgrade` is the new version.

Development versions, such as `1.3.1-dev.20261124T055403Z`, are skipped. Set `include_development: true` to post
them.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `SLACK_BOT_TOKEN` | The bot token of the [Slack app](../../README.md#slack). Posts to the channels in the config. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, when there is no Slack app. Posts every message to its one channel. |
| `TEAMS_WEBHOOK_URL` | A Microsoft Teams Workflows webhook URL, from the "Post to a channel when a webhook request is received" template, or the variable that `teams.webhook_url_env` names. Required with `teams.enabled`. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key stops the automation at start.

| Key | Default | |
|---|---|---|
| `include_development` | `false` | Post development versions too. |
| `slack.channel` | | The channel for release notes, with `SLACK_BOT_TOKEN`. |
| `teams.enabled` | `false` | Post to Microsoft Teams. |
| `teams.webhook_url_env` | `TEAMS_WEBHOOK_URL` | The variable that holds the Teams webhook URL. |

Post to Slack (`SLACK_BOT_TOKEN` and `slack.channel`, or `SLACK_WEBHOOK_URL`), to Teams, or to both. With neither, the automation does not start.

The service account needs `repo:view` on the bundle repositories, and view access to the projects whose
instances you want listed. Instances in projects it cannot view are left out.

## Event destination

```bash
mass event destination create \
  --name release-notes \
  --url https://automations.example.com/hooks/massdriver \
  --event-type repo.published \
  --attribute md-repo-artifact-type=bundle
```

To post only some bundles, add a condition, such as
`--condition '.data.entity.name | startswith("aws-")'`.

Copy the signing secret from the destination into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e SLACK_BOT_TOKEN=xoxb-... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run release-notes
```

## Limits

- The `repo.published` event names the repository, not the version. The automation posts the newest version
  that was published at or before the time of the event. The event also has no way to tell a development
  version apart, so the destination cannot skip them with a condition. The automation skips them.
- Each version is posted once while the automation runs, in memory. Run one replica.
- The automation responds before it posts. A failed post is logged, and Massdriver does not send the event again.
- Slack shows the first 2,900 characters of the changelog and of each instance list.
- Slack and Teams do not show Markdown headings. The automation shows them in bold.
