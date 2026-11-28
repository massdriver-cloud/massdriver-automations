# oncall

On an interval, reads who is on call now in PagerDuty or incident.io, and writes them to the on-call widget of
Massdriver environments, lowest escalation level first. Routes in the config choose which schedules and
escalation policies show on which environments. A route can also write a fixed list, so you can try the widget
without a provider.

## The entry

The key is `md/v1/oncall` in each environment's stash. The value is the widget's people list, in order:

```json
{
  "title": "Billing on call",
  "entries": [
    {
      "name": "Jo Park",
      "image_url": "https://secure.gravatar.com/avatar/1a2b3c.png?d=mm&r=PG",
      "email": "jo@tendwell.example",
      "handles": ["pagerduty:PJO1234"]
    }
  ]
}
```

| Field | PagerDuty | incident.io |
|---|---|---|
| `title` | The route's `title`. Left out when the route has none. | The same. |
| `entries[].name` | The user's name. | The user's name. |
| `entries[].image_url` | The user's avatar. | Left out: incident.io has no avatar in its API. |
| `entries[].email` | The user's email. | The user's email. |
| `entries[].handles` | `pagerduty:<user ID>`. | `incidentio:<user ID>`. |

A route with `static` writes its list as it is.

## Order

- **PagerDuty**: the automation lists the on-call entries of the route's schedules and of its escalation policies,
  with one query for each kind, and orders them by escalation level. A schedule's people are at the level where
  a policy uses that schedule.
- **incident.io**: incident.io has no levels across schedules, so each schedule, then each escalation path, is one
  level in the order of the config. A schedule lists its current shifts. An escalation path lists its current
  responders.

Each person appears once, at their lowest level. The list has at most `max_people` people.

## How it writes

- It writes an environment's entry when the list changed. An entry that does not change is written again in the
  second cycle after its last write.
- Every write sets a TTL of three intervals. When the automation stops, its entries expire.
- When a provider query fails, the automation does not write that route's environments, so nobody seems to go off
  call. The last entry stays until its TTL.
- A route with nobody on call writes nothing. The widget needs at least one person.

The automation keeps what it wrote in memory, and the bundles run one replica. A second replica is safe: both write
the same value.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `PAGERDUTY_TOKEN` | Required when a route has `pagerduty`, or the variable that `pagerduty.token_env` names. A read-only PagerDuty REST API key. |
| `INCIDENT_IO_API_KEY` | Required when a route has `incident_io`, or the variable that `incident_io.token_env` names. An incident.io API key with `schedules.view` and `escalation_paths.view`. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

```yaml
interval: 2m
routes:
  - environments: [billing-prod, billing-staging]
    title: Billing on call
    pagerduty:
      escalation_policies: [PD4E5F6]
  - environments: [visits-prod]
    incident_io:
      schedules: [01HXYZPRIMARY000000000000]
  - environments: [sandbox-dev]
    title: Demo on call
    static:
      - name: Jo Park
        email: jo@tendwell.example
        handles: [slack:@jo]
```

| Key | Default | |
|---|---|---|
| `interval` | `2m` | How often to read the providers and write. A Go duration, at least `30s`. |
| `label` | `On call` | The entry's name in the Massdriver UI. |
| `pagerduty.api_url` | `https://api.pagerduty.com` | For an EU account, `https://api.eu.pagerduty.com`. |
| `pagerduty.token_env` | `PAGERDUTY_TOKEN` | The variable that holds the PagerDuty API key. |
| `incident_io.api_url` | `https://api.incident.io` | The incident.io API. |
| `incident_io.token_env` | `INCIDENT_IO_API_KEY` | The variable that holds the incident.io API key. |
| `routes[].environments` | | Required. Environment identifiers, such as `billing-prod`. An environment can be in one route only. |
| `routes[].title` | | The card's title, up to 64 characters. |
| `routes[].max_people` | `10` | The most people, from 1 to 25. |
| `routes[].pagerduty.schedules` | | PagerDuty schedule IDs. |
| `routes[].pagerduty.escalation_policies` | | PagerDuty escalation policy IDs. |
| `routes[].incident_io.schedules` | | incident.io schedule IDs. |
| `routes[].incident_io.escalation_paths` | | incident.io escalation path IDs. |
| `routes[].static` | | 1 to 25 people, written as they are. Each has at least one of `name` (up to 64 characters), `image_url` (http or https), `email` (one plain address), and `handles` (1 to 10, such as `slack:@jo` or `pagerduty:jo@example.com`). |

A route sets exactly one of `pagerduty`, `incident_io`, or `static`. A `pagerduty` or `incident_io` route lists at
least one ID.

## Permissions

The service account needs `stash:write` on each environment in `routes`.

## Event destination

The automation reads the providers on an interval, and acts on no event. Every automation receives events from a
destination, so create one that sends nothing:

```bash
mass event destination create \
  --name oncall \
  --url https://oncall.example.com/hooks/massdriver \
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
  -e PAGERDUTY_TOKEN=... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run oncall
```

## Limits

- The stash API is alpha. Its names start with `alpha` and can change.
- A PagerDuty query reads at most 1,000 on-call entries.
- Opsgenie is not supported. Atlassian is shutting it down.
