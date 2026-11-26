# bundle-scanner

When a new bundle version is published, pull it from the Massdriver OCI registry and scan its IaC with
[Trivy](https://trivy.dev) (`trivy config`) and [Checkov](https://www.checkov.io). Post a summary of the findings
to Slack.

The `repo.published` event names the bundle but not the version. The automation scans the newest version of the
bundle that was published at or before the time of the event.

A version fails when a finding has the severity in `fail_severity` or higher. Checkov reports no severity
without a Prisma Cloud API key, so its findings count as `unrated` and do not fail a version. The Slack message
has the count for each severity and lists the first 10 findings that cause a failure.

Scans run one at a time, after the automation responds to Massdriver. Each bundle version is scanned once.

### Scan attribute

Set `result_attribute` to record the result on the bundle's repo as the attribute `<key>=passed` or
`<key>=failed`. The automation keeps the repo's other attributes. A scan that cannot finish does not change the
attribute.

The attribute is on the repo, not on the bundle version, so it shows the result of the newest scanned version
only. Bundle versions do not carry attributes of their own. Repo attributes do not cascade to instances, so a
policy on `instance:deploy` cannot use the attribute to block a deploy of a failed version. Use the attribute to
find failed bundles, and use the Slack message to act.

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

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key stops the automation at start.

| Key | Default | |
|---|---|---|
| `scanners` | `[trivy, checkov]` | The scanners to run. |
| `fail_severity` | `HIGH` | `UNKNOWN`, `LOW`, `MEDIUM`, `HIGH`, or `CRITICAL`. A version fails at this severity or above. |
| `result_attribute` | | The repo attribute that records the result. Empty sets no attribute. |
| `timeout` | `10m` | The longest time for one bundle version: the pull and every scanner. |
| `slack.channel` | | The channel for results, with `SLACK_BOT_TOKEN`. Without a channel or a webhook, results go to the logs only. |

The service account needs `repo:view` and `repo:pull` on the bundle repos. With `result_attribute`, it also needs `repo:update`, and the key must be a custom attribute of your organization for repos.

A `.checkov.yml` at the root of the bundle applies to the Checkov scan. Trivy uses the checks built into its pinned version.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name bundle-scanner \
  --url https://bundle-scanner.example.com/hooks/massdriver \
  --event-type repo.published \
  --condition '.data.entity.artifact_type == "application/vnd.massdriver.bundle.v1+json"'
```

To scan one bundle only, add `--attribute md-repo=aws-aurora`.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

The scanners are not in the `massdriver-automations` image. Build the image that adds them, from the root of the
repository:

```bash
docker build -f automations/bundle-scanner/Dockerfile -t bundle-scanner .
docker run -p 8080:8080 \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e SLACK_BOT_TOKEN=xoxb-... \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  bundle-scanner
```

The image pins Trivy 0.76.1 and Checkov 3.4.3. Change the versions in the `Dockerfile`.

## Limits

- The queue holds 100 bundle versions, in memory. When it is full, the automation responds `500`, and Massdriver
  sends the event again later. A restart loses the queue.
- Duplicate events are dropped by id for one hour, and each version is scanned once per process. Run one replica.
- A failed pull or scanner is posted to Slack and logged. Massdriver does not send the event again.
- A bundle larger than 512 MiB is not scanned.
