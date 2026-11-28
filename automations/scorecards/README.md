# scorecards

Scores instances against scorecards, and writes the scores and the facts they use to the stash, where Massdriver
widgets show them. A scorecard is a list of checks. Each check reads one stash entry, such as the drift result
that [drift](../drift) writes or the Checkov counts that [log-results](../log-results) writes, or the instance
itself, and passes when a jq expression is true. The score is the part of the decided checks that passed, and the
level is the highest one the score reaches. Each environment and project gets a roll-up of its instances.

Every event and every interval reads the current state from the API and computes everything again. A late,
repeated, or lost event cannot leave an old value behind: the next event or interval writes the right one.

## The entries

| Key | On | |
|---|---|---|
| `md/v1/scorecard/<name>` | instance | The checks of one instance, its score, and its level. |
| `md/v1/scorecard/<name>` | environment, project | How many instances are at each level, the lowest level, and the level of the mean score. |
| `md/v1/delivery` | instance | Deployments in the window, how many failed, rolled back, or were started by a service account, the failure rate, the median duration, and how the last one ended. |
| `md/v1/reliability` | instance | Alarms, how many fire now, how often they fired in the window, the time spent in alarm, and the median recovery time. |
| `md/v1/currency` | instance | The bundle version the instance runs, the latest version, and how many versions and days it is behind. |
| `md/v1/pending` | instance | Whether the instance's params or resolved version differ from what was last deployed. |
| `md/v1/parity` | environment | The differences from a reference environment: components on one side only, other bundle versions, and params that differ. |
| `md/v1/ownership` | environment, project | The groups that own it, and how many people are in them. |

An instance scorecard:

```json
{
  "tool": "scorecards",
  "level": "silver",
  "score": 82,
  "passed": 9,
  "failed": 2,
  "unknown": 1,
  "checks": [
    {"id": "no-drift", "status": "passed"},
    {"id": "has-alarms", "status": "failed"},
    {"id": "tests", "status": "unknown", "message": "No value for md/v1/tests."}
  ],
  "evaluated_at": "2026-11-27T22:40:03Z"
}
```

A roll-up:

```json
{
  "tool": "scorecards",
  "level": "silver",
  "score": 84.5,
  "instances": 4,
  "levels": {"gold": 1, "silver": 2, "none": 1},
  "lowest_level": "none",
  "evaluated_at": "2026-11-27T22:40:03Z"
}
```

## Checks

A check is `passed`, `failed`, or `unknown`. It is unknown when its entry is missing, expired, or older than
`max_age`, or when its expression fails or returns nothing. Missing data is not a failure: an unknown check counts
in `unknown`, and not in the score. A scorecard whose checks are all unknown has the level `none` and the score 0.

The expression gets the entry's value: a JSON entry as an object, a TEXT entry as a string. It passes when it
returns anything but `false` or `null`, as in jq. It cannot read environment variables, and it stops after two
seconds.

| Input | Reads |
|---|---|
| `stash: <key>` | The key on the instance. |
| `stash: {key: <key>, scope: deployment}` | The key on the instance's latest provision, such as `md/v1/compliance/src`. |
| `stash: {key: <key>, scope: environment}` | The key on the instance's environment, such as `md/v1/oncall`. |
| `stash: {key: <key>, scope: project}` | The key on the instance's project. |
| `query: instance` | The instance: `id`, `status`, `deployedVersion`, `resolvedVersion`, `firingAlarmCount`, `effectiveAttributes`, `params`, `environment`, and `project`. |

A check can read the facts that scorecards writes, such as `md/v1/delivery`. It reads the values just computed,
not the ones in the stash.

Examples:

```yaml
- id: no-drift                 # drift ran within a week and found nothing
  stash: md/v1/drift
  pass: .status == "none"
  max_age: 7d
- id: checkov-clean            # log-results counted no failed Checkov checks in the last provision
  stash: {key: md/v1/compliance/src, scope: deployment}
  pass: .failed == 0
- id: recently-deployed        # deployed in the delivery window
  stash: md/v1/delivery
  pass: .deployments > 0
- id: on-call-set
  stash: {key: md/v1/oncall, scope: environment}
  pass: (.entries | length) > 0
- id: has-alarms
  stash: md/v1/reliability
  pass: .alarms > 0
```

## Which instances

A scorecard scores the instances whose effective attributes match its `match`. An instance that was never
deployed, or was decommissioned, has no infrastructure to score and is skipped. The roll-up of an environment or a
project counts the instances it has that the scorecard scores.

## How the facts are counted

- **delivery** counts the provisions and decommissions created in the window that ran: completed, failed, or
  aborted. A rejected deployment never ran, so it is only `last_status`. A rollback is a completed provision of a
  lower version than the completed provision before it. `automated` counts the deployments that a service account
  created or proposed.
- **reliability** reads the alarm state changes of the window from the audit log. An alarm that was firing when
  the window started counts from the start of the window.
- **currency** compares a stable version with the stable versions, by semantic version. A development build, such
  as `1.2.0-dev.20261126T214340Z`, is newer work than the `1.2.0` it is named after, so it is compared with every
  version published after it.
- **pending** compares the instance's params with those of its latest provision, and its resolved version with
  the deployed one.
- **ownership**: a group owns an environment when its policies allow `ownership.environment_action` there, and a
  project when they allow `ownership.project_action` in any of its environments. Organization admin groups can do
  everything, so they own nothing. `members` counts each person once.
- **parity** uses the environment comparison of the API, for each pair in `parity`.

## How it writes

- An entry is set when its value changes, and again once an interval has passed, so `evaluated_at` is never more
  than one interval old. Entries have no TTL.
- Before its first write of an entry, the automation reads it, so a restart does not rewrite every entry.
- A failed read leaves out the fact it was for, and the other facts and the scores are still written. A failed
  write is logged, and the next event or interval writes it again. Neither makes Massdriver send the event again.
- The automation answers each event at once, and computes in the background. An instance named by many events
  while its computation waits is computed once.

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
| `interval` | `1h` | How often every scored instance is computed again. Catches what time changes, such as a deployment that leaves the window or an entry that passes `max_age`. A Go duration, at least `1m`. |
| `delivery.window` | `30d` | The days of deployments that `md/v1/delivery` counts. |
| `reliability.window` | `30d` | The days of alarm changes that `md/v1/reliability` counts. |
| `ownership.environment_action` | `environment:deploy` | The action that makes a group an owner of an environment. |
| `ownership.project_action` | `project:update` | The action that makes a group an owner of a project. |
| `parity[].environment` | | An environment to compare, such as `api-prod`. |
| `parity[].reference` | | The environment it is compared with, such as `api-staging`. |
| `scorecards[].name` | | Required. 1 to 63 lowercase letters, digits, `_`, and `-`. The key is `md/v1/scorecard/<name>`. |
| `scorecards[].match` | | Effective attributes an instance must have, such as `md-environment: [prod, staging]`. A list matches any of its values. Empty matches every instance. |
| `scorecards[].checks[].id` | | Required. 1 to 64 characters, unique in the scorecard. |
| `scorecards[].checks[].stash` | | A key, or `{key, scope}`. See [Checks](#checks). |
| `scorecards[].checks[].query` | | `instance`. A check sets `stash` or `query`. |
| `scorecards[].checks[].pass` | | Required. A jq expression. |
| `scorecards[].checks[].max_age` | | A stash entry older than this is unknown, such as `2d` or `12h`. |
| `scorecards[].levels` | `gold: 100`, `silver: 80`, `bronze: 50` | Up to 9 level names and the score each needs, from 0 to 100. A score below every level is `none`. |
| `results.prefix` | `md/v1` | Replaces `md/v1` in the keys that scorecards writes. The server rejects an `md/` key until its widget catalog has it; a prefix outside `md/`, such as `results/v1`, shows each value as a labeled metadata row until then. |

## Permissions

The service account needs to view the projects, environments, instances, deployments, alarms, audit logs, and
bundles that it scores, and `stash:write` on those instances, environments, and projects. Ownership needs
`organization:manageGroups` to read what each group's policies allow; without it, the ownership entries are not
written and the log says `job failed` for `ownership`.

## Event destination

```bash
mass event destination create \
  --name scorecards \
  --url https://scorecards.example.com/hooks/massdriver \
  --event-type deployment.completed --event-type deployment.failed \
  --event-type deployment.aborted --event-type deployment.rejected \
  --event-type 'alarm.*' --event-type alarm_state.created \
  --event-type 'instance.*' --event-type repo.published \
  --event-type 'group_membership.*' \
  --event-type stash_entry.set --event-type stash_entry.deleted \
  --condition '(.type | startswith("stash_entry.") | not) or (.data.entity.key | IN("md/v1/drift", "md/v1/compliance/src", "md/v1/oncall"))'
```

List in the condition the keys your checks read, so that other automations' frequent writes, such as stash-sync's
traffic every minute, are not sent. The automation drops a stash event of a key no check reads, and of the keys it
writes itself, on its own as well.

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
- A list reads at most 5,000 items, such as instances, deployments in the window, or versions of a bundle.
- Ownership asks the API once for each environment and group that is not an organization admin group, each
  interval and each `group_membership.*` event.
