# scorecards

Scores instances against scorecards, such as production readiness, and writes the scores to the stash. It also
computes the facts that scorecards often check (deployments, alarms, bundle versions, pending changes, parity
with another environment, and owners) and writes them to their own keys. The Massdriver UI shows each of these
keys as a widget.

The automation keeps no state. On each event, it reads the current state of the instance from the API and
computes its facts and scores again. A late event, or an event that Massdriver sends twice, writes the same
values, so nothing changes. A pass every `interval` computes every scored instance again, for what time changes,
such as deployments that leave the window, and for events that never arrived.

## The keys

| Key | Stash | What it holds |
|---|---|---|
| `md/v1/scorecard/<name>` | instance | The level, the score, the counts of passed, failed, and unknown checks, and each check with its status. |
| `md/v1/scorecard/<name>` | environment, project | A roll-up of the instances: how many are at each level, the lowest level, and the mean score with its level. |
| `md/v1/delivery` | instance | Deployments in the window: how many ran, failed, were rolled back, and were started by a service account, the failure rate, the median duration, and the last status. |
| `md/v1/reliability` | instance | Alarms: how many there are and how many fire now, how many times one fired in the window, the seconds in alarm, the median recovery time, and when one last fired. |
| `md/v1/currency` | instance | The bundle version the instance runs, the latest stable version, how many stable versions are newer, and for how many days a newer one has been out. |
| `md/v1/pending` | instance | Whether the next deployment would change the instance, and when it was last deployed. |
| `md/v1/parity` | environment | The differences from a reference environment: instances on one side only, other bundle versions, and params with other values. Up to 50 are listed. |
| `md/v1/ownership` | environment, project | The groups that own it, and how many people are in them. |

Only the instances that a scorecard matches get facts and scores. Every value has `tool: scorecards`.

How each fact is computed:

- **Delivery** counts the provision and decommission deployments created in the window that ran: completed,
  failed, or aborted. A rejected deployment never ran, so it counts only as `last_status`. A rollback is a
  completed provision of a lower bundle version than the completed provision before it. `automated` counts the
  deployments that a service account created, from the audit log. `failure_rate` and `median_duration_seconds`
  are left out when no deployment ran.
- **Reliability** reads the `alarm_state.created` records of the window from the audit log. An alarm whose first
  change in the window is to `OK` was firing when the window started. An alarm that has fired since before the
  window, and has not changed, was in alarm the whole window.
- **Currency** compares the deployed version with the stable versions of the bundle. It is left out for an
  instance that was never deployed.
- **Pending** compares the instance's params with the params of its latest provision, and the version it
  resolves to with the version it runs. The API has no time for a params change, so `changed_at` is left out.
- **Parity** uses `compareEnvironments` for each pair in `parity`.
- **Ownership:** a group owns an environment when its policies allow `ownership.environment_action` there, and a
  project when they allow `ownership.project_action`. Organization admin groups can do everything, so they do
  not count as owners.

A fact whose reads fail is logged with `fact skipped` and left out. The other facts and the scores are still
written, and a check of the missing fact is `unknown`.

## Scores

Each check reads one input and passes when its `pass` jq expression returns anything but `false` or `null`. The
input is a stash value (a JSON value as an object, a TEXT value as a string), or the instance itself.

A check is `unknown`, not failed, when its stash key is not set, has expired, or was last set longer ago than
`max_age`, or when its expression fails. The score counts only the checks that passed or failed:

```text
score = passed / (passed + failed) × 100
```

The level is the highest level whose threshold is at most the score. A score below every threshold, or a
scorecard whose checks are all unknown, has the level `none` and the score 0.

An environment or project roll-up counts the scorecards of its instances that the scorecard matches. Its score
is their mean, and its level is the level of that mean. `lowest_level` is the lowest level of any of them.

## How it writes

- It writes a key only when its value changed. `evaluated_at` and `checked_at` do not count as a change, so they
  say when the value last changed.
- Before its first write of a key, it reads the key, so a restart does not write every key again.
- Entries have no TTL. Each event and each interval computes them again.
- Jobs wait in a queue by instance, environment, or project. A job that many events name waits once. Four jobs
  run at a time.
- A failed write is logged, and the next event or interval writes it again.

## Events

| Event | Computes again |
|---|---|
| `deployment.completed`, `.failed`, `.aborted`, `.rejected` | The instance |
| `alarm_state.created`, `alarm.created`, `.updated`, `.deleted` | The instance |
| `instance.created`, `.updated`, `.moved`, `.orphaned`, `.copied` | The instance |
| `instance.deleted` | The roll-ups of its environment and project |
| `repo.published` | Every instance of the bundle |
| `group_membership.added`, `.removed` | Ownership |
| `stash_entry.set`, `.deleted` | The instance whose stash, or whose deployment's stash, has the key. A key on an environment or project computes each of its instances again. Only keys that a check reads count. |

After an instance, the automation computes the roll-ups of its environment and project, and the parity of its
environment.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

| Key | Default | |
|---|---|---|
| `interval` | `1h` | How often every scored instance is computed again. At least `1m`. |
| `delivery.window` | `30d` | The days of deployments that delivery counts. Whole days. |
| `reliability.window` | `30d` | The days of alarm changes that reliability counts. Whole days. |
| `ownership.environment_action` | `environment:deploy` | The action that makes a group an environment's owner. |
| `ownership.project_action` | `project:update` | The action that makes a group a project's owner. |
| `parity[].environment`, `parity[].reference` | | An environment ID and the environment ID it is compared with. |
| `scorecards[].name` | | Required. Unique. 1 to 63 lowercase letters, digits, `_`, and `-`. The key is `md/v1/scorecard/<name>`. |
| `scorecards[].match` | | Effective attributes that an instance must have. A list matches any of its values. Empty matches every instance. |
| `scorecards[].checks[].id` | | Required. Unique in the scorecard. Up to 64 characters. |
| `scorecards[].checks[].stash` | | A key on the instance, such as `md/v1/drift`, or `{key, scope}` with `scope` one of `instance`, `deployment` (the latest provision), `environment`, or `project`. |
| `scorecards[].checks[].query` | | `instance`, in place of `stash`: the instance's `id`, `status`, `deployedVersion`, `resolvedVersion`, `firingAlarmCount`, `effectiveAttributes`, `params`, `environment`, and `project`. |
| `scorecards[].checks[].pass` | | Required. A jq expression. It cannot read environment variables. |
| `scorecards[].checks[].max_age` | | For a `stash` check: older input is unknown. Such as `2d` or `12h`. |
| `scorecards[].levels` | `gold: 100`, `silver: 80`, `bronze: 50` | Level names and the score each needs, from 0 to 100. Up to 9 levels. |

```yaml
scorecards:
  - name: production-readiness
    match:
      md-environment: prod
    checks:
      - id: no-drift
        stash: md/v1/drift
        pass: .status == "none"
        max_age: 2d
      - id: tests-passed
        stash: {key: md/v1/tests, scope: deployment}
        pass: .failed == 0
      - id: has-alarms
        stash: md/v1/reliability
        pass: .alarms > 0
```

The [telehealth example](../../examples/telehealth/scorecards.yaml) scores every prod instance of Tendwell Health.

The service account needs:

- `project:view` on the projects of the scored instances, to read instances, deployments, alarms, bundle
  versions, the stash, and the audit log
- `stash:write` on the scored instances, their environments, and their projects
- `organization:manageGroups`, to read which groups own an environment

## Event destination

```bash
mass event destination create \
  --name scorecards \
  --url https://scorecards.example.com/hooks/massdriver \
  --event-type deployment.completed --event-type deployment.failed --event-type deployment.aborted \
  --event-type deployment.rejected --event-type alarm_state.created --event-type alarm.created \
  --event-type alarm.updated --event-type alarm.deleted --event-type instance.created \
  --event-type instance.updated --event-type instance.deleted --event-type instance.moved \
  --event-type instance.orphaned --event-type instance.copied --event-type repo.published \
  --event-type group_membership.added --event-type group_membership.removed \
  --event-type stash_entry.set --event-type stash_entry.deleted \
  --condition '(.type | startswith("stash_entry.") | not) or (.data.entity.key | test("^md/v1/(scorecard/|(delivery|reliability|currency|pending|parity|ownership)$)") | not)'
```

The condition drops the stash events of the keys that scorecards writes, so its own writes do not start more
work. The automation ignores those events too, and the stash events of keys that no check reads.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  ghcr.io/massdriver-cloud/massdriver-automations:latest run scorecards
```

## Limits

- The stash API is alpha. Its names start with `alpha` and can change.
- Delivery and reliability read the audit log, so they count only what the audit log still keeps.
- Each list reads at most 50 pages of 100 items.
- Ownership asks each group about each environment, so it makes groups × environments calls each interval.
- A jq expression runs for at most 2 seconds.
