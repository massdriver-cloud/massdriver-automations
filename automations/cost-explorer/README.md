# cost-explorer

Serves a dashboard of the cloud costs of every instance in your organization. Group and filter the costs by any
instance attribute, such as project, environment, component, bundle, or your own `team` and `cost_center`
attributes, and see how daily spend changes.

The dashboard has:

- the total of the chosen measure for the instances in view: last month, monthly average, daily average, or
  last day, and the change in daily spend over the last 7 days
- the projected cost of this month, from the daily average
- a ranked list of the groups. Select a group to filter to it.
- a daily cost chart, stacked by group, over 7, 30, 90 days, or all history
- a table of the groups, with each group's instances and links to them in Massdriver
- a CSV download of the instances in view

Filters, grouping, the measure, and the date range are in the URL, so you can share a view.

## How it works

1. At start, every `refresh` interval, and 30 seconds after a provision or decommission completes, it reads the
   cost summary of every instance from the Massdriver API.
2. It records each instance's last-day cost once a day in `STATE_DIR`. The daily chart comes from this history.
   An instance that was deleted stays in the history under the group it had.
3. It serves the dashboard at `/` and the data at `/api/costs.json`, behind basic auth.

Only the attributes in `dimensions` reach the browser.

## Configuration

Every key is optional. See [config.example.yaml](config.example.yaml).

| Key | Default | |
|---|---|---|
| `title` | `Cloud costs` | The dashboard's title. |
| `dimensions` | `md-project`, `md-environment`, `md-component`, `bundle` | The instance attributes to group and filter by. `bundle` is the bundle name. |
| `refresh` | `1h` | How often to read the costs. At least `1m`. |
| `history_days` | `90` | Days of daily history to keep, from 1 to 731. |
| `public` | `false` | Serves the dashboard without a password. |
| `username` | `massdriver` | The basic auth user. |
| `massdriver.app_url` | `https://app.massdriver.cloud` | The Massdriver UI, for the instance links. |

## Environment variables

| Variable | |
|---|---|
| `MASSDRIVER_ORGANIZATION_ID`, `MASSDRIVER_API_KEY` | Required. The service account needs `project:view` on the projects whose costs it shows. |
| `MASSDRIVER_SIGNING_SECRET` | Required. The signing secret of the event destination. |
| `COST_EXPLORER_PASSWORD` | The basic auth password. Required unless `public` is `true`. |
| `STATE_DIR` | Where the daily history is kept. Without it, history starts at each restart. |

## Event destination

The destination is optional: without it, costs refresh every `refresh` interval. With it, a new or removed
instance shows up 30 seconds after its deployment.

```bash
mass event destination create \
  --name cost-explorer \
  --url https://costs.example.com/hooks/massdriver \
  --event-type deployment.completed \
  --condition '.data.metadata.deployment_action == "PROVISION" or .data.metadata.deployment_action == "DECOMMISSION"'
```

Copy the signing secret from the destination into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -v "$PWD/state:/state" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e COST_EXPLORER_PASSWORD=... \
  -e STATE_DIR=/state \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run cost-explorer
```

Open `http://localhost:8080` and sign in as `massdriver`.

## Limits

- Massdriver gives four numbers for each instance: last day, daily average over 7 days, last month, and monthly
  average. The daily chart is built from the last-day number, read once a day, so it starts when the service
  first runs and has no data from before.
- Costs come from the cloud billing integrations that your organization has set up. An instance without billing
  data shows `-`.
- The dashboard shows amounts in the currency of the first instance that has one. It does not convert currencies.
- Run one replica. History is in memory and in one file.
