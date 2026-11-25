# freeze-guard

Abort each `PROVISION` or `DECOMMISSION` deployment that starts inside a freeze window. A window is weekly, such as
every weekend, or a fixed range, such as a holiday. Each window applies to the instances whose attributes match.

A deployment whose message contains the override token, `[freeze-override]` by default, is not aborted.

The automation acts on two events:

- `deployment.enqueued`: Massdriver hands the deployment to the provisioner. Every deployment that runs sends it:
  a direct deploy, an approved proposal, and a deployment that waited behind another one.
- `deployment.created` with status `PENDING`: a deployment that waits behind another deployment on the same
  instance. An abort at this point stops the deployment before Massdriver hands it to the provisioner.

The time of the event decides the freeze, not the time the event arrives. Each deployment is aborted one time.

Plans are not aborted. A proposal is not aborted when it is created. Its approval sends `deployment.enqueued`.

When `SLACK_WEBHOOK_URL` is set, each abort posts a message to that Slack incoming webhook.

## Windows file

YAML or JSON. `timezone` applies to every window that does not set its own. The default is `UTC`.

```yaml
timezone: America/New_York
windows:
  - name: weekend
    match:
      md-environment: prod
    weekly:
      - from: Fri 17:00
        to: Mon 08:00
  - name: thanksgiving
    match:
      md-environment: prod
    from: 2026-11-26
    to: 2026-11-30 08:00
  - name: payments-quarter-close
    timezone: Europe/London
    match:
      md-project: payments
    from: 2026-12-31T00:00:00Z
    to: 2027-01-02T00:00:00Z
```

| Field | |
|---|---|
| `name` | Shown in the log and the Slack message. |
| `match` | Attributes that the instance must have, each with exactly this value. Empty matches every instance. |
| `timezone` | An IANA time zone, such as `America/New_York`. |
| `weekly` | A list of ranges, each `from` and `to` as `<day> HH:MM`. A range whose `to` is earlier in the week than its `from` continues into the next week. |
| `from`, `to` | A fixed range: `YYYY-MM-DD`, `YYYY-MM-DD HH:MM` in the window's time zone, or RFC 3339. `to` is not included. |

A window has `weekly` or `from` and `to`, not both.

## Configuration

| Variable | Default | |
|---|---|---|
| `MASSDRIVER_ORGANIZATION_ID` | | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | | Required. An `md_` access token of a service account. |
| `MASSDRIVER_SIGNING_SECRET` | | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_URL` | `https://api.massdriver.cloud` | The URL of your Massdriver installation. |
| `FREEZE_WINDOWS_FILE` | | Required. The path of the windows file. |
| `FREEZE_OVERRIDE_TOKEN` | `[freeze-override]` | Text in a deployment message that lets the deployment run during a freeze. |
| `SLACK_WEBHOOK_URL` | | A Slack incoming webhook URL for abort messages. |
| `PORT` | `8080` | |

The service account needs `instance:deploy` to abort a `PROVISION`, and `instance:decommission` to abort a
`DECOMMISSION`, on the instances that the windows cover.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name freeze-guard \
  --url https://freeze-guard.example.com/hooks/massdriver \
  --event-type deployment.created \
  --event-type deployment.enqueued \
  --attribute md-environment=prod \
  --condition '.data.metadata.deployment_action != "PLAN" and (.data.entity.status == "PENDING" or .data.entity.status == "APPROVED")'
```

The condition drops plans and proposals, so Massdriver marks them `SKIPPED` and does not send them. The attribute
must cover every window's `match`. Remove `--attribute` when the windows cover more than production.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/windows.yaml:/etc/freeze-guard/windows.yaml:ro" \
  -e FREEZE_WINDOWS_FILE=/etc/freeze-guard/windows.yaml \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e SLACK_WEBHOOK_URL=https://hooks.slack.com/services/... \
  massdrivercloud/massdriver-automations:latest run freeze-guard
```

The windows file is read at start. Restart the service after you change it.

## Limits

- An abort does not stop a provisioner run. Massdriver hands a deployment to the provisioner before it sends
  `deployment.enqueued`. `abortDeployment` marks the deployment `ABORTED` and starts the next deployment in the
  queue, but it does not cancel the provisioner run, and Massdriver ignores the run's later status reports. A
  run that starts can still change infrastructure, and the instance keeps the status it had before the deployment.
  Only a deployment that is aborted while it waits behind another deployment (`deployment.created`) is stopped
  before it runs.
- There is no event before Massdriver hands a direct deploy or an approved proposal to the provisioner. To stop
  those deployments in a freeze, propose deployments with `proposeDeployment` instead of deploying, and do not
  approve during the freeze.
- `abortDeployment` takes no reason. The reason is in the log line `aborted` and the Slack message only.
- When `abortDeployment` refuses, for example because the deployment already completed, the refusal is logged and
  the event is not retried.
- Aborted deployments are remembered in memory for 24 hours. Run one replica.
