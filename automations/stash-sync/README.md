# stash-sync

On an interval, reads metrics from AWS CloudWatch and Prometheus and writes them to Massdriver stash entries. The
widget keys under `md/v1/` then show on the canvas for everyone who can view the project:

| Key | Stash | Format | Widget |
|---|---|---|---|
| `md/v1/health` | instance | TEXT, 1 to 64 characters | A dot on the instance. `healthy`, `ok`, and `up` are green; `degraded`, `warning`, and `recovering` are yellow; `down`, `failed`, and `error` are red. |
| `md/v1/usage` | instance | JSON `{value, max, label?}` | A bar on the instance. |
| `md/v1/traffic` | instance | JSON `{rps, errors_5m, p95_ms?}` | Traffic on the lines that leave the instance. |
| `md/v1/banner` | environment | JSON `{message, level?}` | A message across the top of the environment. `level` is `info`, `warning`, or `error`. |
| `md/v1/links` | environment | JSON `{links: [{label, url}]}` | Up to 8 links in the environment's header. |

The automation runs in your cloud, next to your metrics, with your credentials. Massdriver does not need access to
CloudWatch or Prometheus.

In simulate mode, it writes changing values from a scenario, with no cloud access, for demos and tests.

## How it writes

- Each cycle, the automation runs every query, builds the value of each entry, and writes the entries whose value
  changed. Several rules can fill the fields of one JSON entry, which is written once.
- An entry that does not change is written again in the second cycle after its last write.
- Every write sets a TTL of three intervals. When the automation stops, its entries expire, and their widgets go
  away.
- Before it writes an `md/` key, the automation checks the value against the key's JSON Schema from
  `alphaWidgetCatalog`. A field that the schema says is an integer, such as `errors_5m`, is rounded. A value that
  does not match is logged and skipped. An `md/` key that is not in the catalog, such as the unversioned `md/health`,
  is skipped.
- A query that fails is logged, and the other entries are still written. A JSON entry leaves out the fields whose
  queries failed. A query with no data uses the rule's `default`, or is skipped.
- Values have at most two decimal places, so small changes do not cause a write.

The automation keeps what it wrote in memory, and the bundles run one replica. A second replica is safe with sync
mode: each one writes the whole value of an entry, so the last write wins, and both write the same value. In
simulate mode, two replicas write different random values to the same entries.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `AWS_REGION`, `AWS_ACCESS_KEY_ID`, ... | The [default AWS credential chain](https://docs.aws.amazon.com/sdkref/latest/guide/standardized-credentials.html): keys, a profile, IRSA, or an ECS task role. Only CloudWatch rules and presets use it. |
| The variables in `token_env` and `password_env` | The Prometheus bearer token or basic auth password. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

| Key | Default | |
|---|---|---|
| `mode` | `sync` | `sync` reads the sources. `simulate` writes values from a scenario. |
| `interval` | `60s` (`5s` in simulate mode) | How often a cycle runs. A Go duration, at least `1s`. |
| `cloudwatch.region` | the credential chain's | The region of the metrics and alarms. |
| `prometheus.url` | | The base URL of the Prometheus HTTP API, such as `http://prometheus:9090`. Required for Prometheus rules and presets. |
| `prometheus.token_env` | | The variable with a bearer token. |
| `prometheus.username` | | A basic auth user. Ignored with a token. |
| `prometheus.password_env` | | The variable with the basic auth password. |
| `prometheus.timeout` | `10s` | How long one query can take. |
| `presets[]` | | Common metrics. See [Presets](#presets). |
| `rules[]` | | Any entry. See [Rules](#rules). |
| `simulate` | | The scenario for simulate mode. See [Simulate](#simulate). |

### Presets

A preset fills one widget key on one instance, with no queries to write. Its settings go in `with`.

```yaml
presets:
  - preset: aws-alb-traffic
    instance: billing-prod-app
    with:
      load_balancer: app/billing-prod-app/0123456789abcdef
```

| Preset | Key | `with` | Reads |
|---|---|---|---|
| `aws-rds-connections` | `md/v1/usage` | `db_instance`, `max`, `label` (default `connections`) | `AWS/RDS` `DatabaseConnections`, average over 1 minute, against `max`. |
| `aws-alb-traffic` | `md/v1/traffic` | `load_balancer`, `target_group` (optional) | `AWS/ApplicationELB`: `RequestCount` per second, `HTTPCode_Target_5XX_Count` over 5 minutes, and `TargetResponseTime` p95 in milliseconds. A period with no requests or errors counts as 0. |
| `aws-alarm-health` | `md/v1/health` | `alarm` | A metric or composite alarm's state: `OK` is `healthy`, `INSUFFICIENT_DATA` is `degraded`, and `ALARM` is `down`. |
| `prometheus-traffic` | `md/v1/traffic` | `job` or `selector`, `requests_metric` (default `http_requests_total`), `code_label` (default `code`), `duration_metric` (default `http_request_duration_seconds`) | The request rate, the 5xx responses in 5 minutes, and the p95 of the duration histogram, over 5 minutes. |

`load_balancer` and `target_group` are the `LoadBalancer` and `TargetGroup` dimensions, the end of each ARN, such
as `app/name/0123456789abcdef`. `selector` is PromQL label matchers, such as `service="claims",namespace="billing"`.

### Rules

A rule writes one entry, or one field of a JSON entry. It has a `target` and exactly one of `cloudwatch`,
`prometheus`, or `value`.

```yaml
rules:
  - target: {instance: billing-prod-claims, key: md/v1/usage, field: value}
    prometheus: {query: sum(claims_queue_depth)}
  - target: {instance: billing-prod-claims, key: md/v1/usage, field: max}
    value: 5000
  - target: {instance: billing-prod-app, key: md/v1/health}
    cloudwatch:
      namespace: AWS/ApplicationELB
      metric: UnHealthyHostCount
      dimensions: {LoadBalancer: app/billing-prod-app/0123456789abcdef}
      stat: Maximum
    default: 0
    text:
      - {below: 1, text: healthy}
      - {below: 2, text: degraded}
      - {text: down}
```

| Key | Default | |
|---|---|---|
| `target.project`, `.environment`, `.component`, `.instance`, `.deployment` | | Exactly one: the stash to write. A deployment is its UUID; the others are identifiers, such as `billing-dev-app`. |
| `target.key` | | Required. Letters, digits, and `_ . / -`, up to 255 characters. |
| `target.field` | | A field of a JSON value. Rules for the same key with different fields make one value. Without a field, the rule writes the whole value, and must be the only rule for its key. |
| `target.format` | `JSON` with a field or an object, `TEXT` otherwise | `TEXT` or `JSON`. |
| `target.label` | | The entry's name in the Massdriver UI. |
| `cloudwatch.namespace`, `.metric`, `.dimensions` | | The metric. |
| `cloudwatch.stat` | `Average` | `Average`, `Sum`, `Minimum`, `Maximum`, `SampleCount`, or a percentile, such as `p95`. |
| `cloudwatch.period` | `60s` | Whole minutes, up to `24h`. The latest data point in the last three periods is used. |
| `cloudwatch.alarm` | | An alarm name, in place of a metric. The state is a number: `OK` is 0, `INSUFFICIENT_DATA` is 1, and `ALARM` is 2. |
| `prometheus.query` | | PromQL that returns a scalar or one series. More than one series is an error: aggregate it, such as with `sum()`. |
| `value` | | A fixed value: a string, number, list, or object. |
| `scale` | | Multiplies the number, such as `1000` for seconds to milliseconds. |
| `default` | | The number to use when the query has no data. |
| `text` | | For a TEXT entry: the first rule whose `below` is more than the number gives the text. A last rule without `below` matches every number. Without `text`, the number is written as text. |

### Simulate

```yaml
mode: simulate
interval: 5s
simulate:
  scenario: postgres-cutover
  duration: 3m
  loop: true
  roles:
    environment: billing-dev
    old_database: billing-dev-claims
    new_database: billing-dev-ledger
```

| Key | Default | |
|---|---|---|
| `simulate.scenario` | | Required. `steady`, `incident`, `postgres-cutover`, or `rollout`. |
| `simulate.duration` | `3m` | How long one pass of the scenario takes. At least `10s`. |
| `simulate.loop` | `true` | Start again after each pass. `false` stops writing after one pass, and the entries expire. |
| `simulate.seed` | the clock | A seed for the random values. The same seed writes the same values. |
| `simulate.roles` | | The environments and instances of the scenario. |

Values move in four ways: a random walk between bounds, bursts of errors, idle stretches with no traffic, and
slow drift, such as usage that climbs past 80% and then 95%.

| Scenario | Roles | What happens |
|---|---|---|
| `steady` | `instances` | Each instance is `healthy`, with traffic and CPU usage that change a little. |
| `incident` | `environment`, `instance`, `instances` (optional) | `instance` goes from `healthy` to `degraded` as latency and usage climb past 80%, then `down` with an error burst and usage past 95%, then `recovering`, then `healthy`. The banner goes from warning to error to info. The other `instances` stay steady. |
| `postgres-cutover` | `environment`, `old_database`, `new_database`, `max_lag` (default 120), `instances` (optional) | The banner shows "Replicating...", then "Catching up: lag Ns", "Cutover in progress", and "Cutover complete". The replication lag is a usage bar on the new database that counts down from `max_lag` seconds. At cutover, traffic on the old database's lines drops to idle while the new database's rises. Each database's health dot follows the phases. |
| `rollout` | `environments` (each with `environment` and `instances`), `advisory` (optional) | Every environment starts `vulnerable`, with the advisory as a warning banner. One environment at a time, its instances go to `patching`, with more errors, and then `patched`. The banner shows the progress, then "Patched". |

The [telehealth example](../../examples/telehealth) has a config for each scenario on the `billing-dev` environment
of Tendwell Health.

## Permissions

The service account needs:

- `stash:write` on the instances, environments, components and projects it writes to

With CloudWatch rules or presets, the AWS credentials need `cloudwatch:GetMetricData` and, for alarms,
`cloudwatch:DescribeAlarms`. CloudWatch bills `GetMetricData` for each metric it returns.

## Event destination

The automation reads its sources on an interval, and acts on no event. Every automation receives events from a
destination, so create one that sends nothing:

```bash
mass event destination create \
  --name stash-sync \
  --url https://stash-sync.example.com/hooks/massdriver \
  --event-type deployment.completed \
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
  -e AWS_REGION=us-east-1 \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run stash-sync
```

## Limits

- The stash API is alpha. Its names start with `alpha` and can change.
- Each rule is one query each cycle, eight at a time. A query that takes longer than 30 seconds fails.
- A CloudWatch metric is often a few minutes behind. The automation uses the latest data point in the last three
  periods.
- The widget catalog is read once, at the first cycle that writes an `md/` key. After a change to the catalog,
  restart the automation.
