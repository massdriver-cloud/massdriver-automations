# log-results

When a deployment finishes, read its logs with rules from the config file, and write what the rules find to the
deployment's stash: the changes of a plan, the outcome of an apply, a Helm release, or the findings of a Checkov
scan. With the default `md/v1` keys, the deployment shows them as widgets; each entry also has a label.

Provisioners will write these results to the stash themselves. Until they do, this automation reads them from the
logs.

The automation does not know which provisioner ran. Each rule has a `detect` pattern, such as `OpenTofu has been
successfully initialized` or `REVISION: \d+`, and writes nothing when the pattern is not in the logs. Rules for
every tool can live in one config file.

## How a rule reads the logs

1. The automation reads the logs of each `deployment.completed` and `deployment.failed` event of the
   [actions](#configuration) you choose, `PROVISION` and `PLAN` by default.
2. It reads the logs once, line by line, with every rule whose `match` and `actions` fit the deployment. Terminal
   colors are removed first, so `Plan: 2 to add` matches although OpenTofu prints `Plan:` in bold.
3. Each pattern is matched against each line. The named groups of a match, such as `(?P<add>\d+)`, are its
   captures. A capture that is a whole number becomes a number; anything else stays text.
4. When the logs end, each rule that detected its tool and matched at least one line builds its value from the
   captures, and the automation writes it to `<prefix>/<key>` on the deployment.

The same deployment always gives the same entries, so a repeated event writes the same values again.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments, and the rules
below for OpenTofu, Terraform, Checkov, Helm, and Bicep. An unknown key, a pattern that does not compile, or a value
that is not valid jq stops the automation at start.

| Key | Default | |
|---|---|---|
| `actions` | `[PROVISION, PLAN]` | The deployment actions whose logs are read: `PROVISION`, `PLAN`, and `DECOMMISSION`. |
| `results.prefix` | `md/v1` | The start of every key. See [Keys and widgets](#keys-and-widgets). |
| `max_log_mb` | `64` | Read at most this many MiB of a deployment's logs. |
| `max_matches` | `1000` | A rule stops reading after this many matches. |
| `rules` | | Required. The [rules](#rules). |

## Rules

```yaml
rules:
  - name: opentofu-changes
    match:
      md-environment: [staging, prod]
    actions: [PROVISION, PLAN]
    detect: (OpenTofu|Terraform) has been successfully initialized
    patterns:
      - 'Plan: (?:(?P<imports>\d+) to import, )?(?P<add>\d+) to add, (?P<change>\d+) to change, (?P<destroy>\d+) to destroy'
      - (?P<no_changes>No changes\.)
    mode: sum
    key: changes/src
    label: OpenTofu changes
    format: json
    value: '{tool: "opentofu", add: (.add // 0), change: (.change // 0), destroy: (.destroy // 0)}'
```

| Field | Default | |
|---|---|---|
| `name` | | Required. Unique. The automation's logs name the rule. |
| `match` | every deployment | Attributes that the deployment's event must have. `md-project`, `md-environment`, `md-component`, `md-instance`, and `md-repo` (the bundle name) are always there. A list matches any one of its values. |
| `actions` | every action in `actions` | The deployment actions this rule reads. |
| `detect` | none | A regular expression that must match a line of the logs, or the rule writes nothing. |
| `patterns` | | Required. Regular expressions in [Go RE2 syntax](https://github.com/google/re2/wiki/Syntax), each matched against every line. Named groups, `(?P<name>...)`, are the captures. |
| `mode` | `last` | Which match gives each capture its value. `last`: the last match. `first`: the first match. `sum`: whole numbers add up across matches, such as the plans of a bundle's steps; text keeps the last match. |
| `key` | | Required. The key under `results.prefix`. Letters, digits, `_`, `.`, `/`, and `-`. |
| `label` | the rule's name | The name of the row in **Deployment metadata**. |
| `format` | `json` | `json` or `text`. |
| `value` | the captures | A [jq](https://jqlang.org/manual/) expression over the captures. Without it, a `json` rule writes the captures as an object, and a `text` rule writes its one capture. |

A pattern matches one line at a time, and a capture that did not take part in a match keeps its earlier value. A
pattern without named groups still counts as a match, which is how `No changes.` gives a plan of zeros.

The `value` expression reads the captures as `.`, such as `.add`, and these variables:

| Variable | |
|---|---|
| `$status` | `succeeded` for `deployment.completed`, `failed` for `deployment.failed`. |
| `$action` | `plan`, `apply`, or `destroy`, for `PLAN`, `PROVISION`, and `DECOMMISSION`. |
| `$matches` | How many pattern matches the rule found. |
| `$deployment_id` | The deployment's ID. |
| `$attributes` | The attributes of the event, such as `$attributes["md-instance"]`. |

A capture that is missing is `null`, so `(.add // 0)` gives a default, and `with_entries(select(.value != null))`
drops the fields that the logs did not have. A decimal stays text, so that a version such as `1.10` does not become
`1.1`. Use `(.cost | tonumber)` for a decimal number. A `text` value must be a string, a number, or a boolean.

The logs do not say which step of a bundle wrote a line. For a bundle with more than one step, use `mode: sum` to
total the steps, or give each bundle a rule with `match: {md-repo: <bundle>}` and its own key.

## Examples

The examples write keys in the shapes of the proposed deployment results: `step/<step>` for how a step ran,
`changes/<step>` for what a plan changes, and `compliance/<step>` for a scanner's findings, each with a `tool`
field. They use `src`, the step name of most bundles.

### OpenTofu and Terraform

```yaml
rules:
  - name: opentofu-step
    detect: (OpenTofu|Terraform) has been successfully initialized
    patterns:
      - (?P<tool>OpenTofu|Terraform) has been successfully initialized
      - (?:OpenTofu|Terraform) v(?:ersion:\s*)?(?P<tool_version>\d+\.\d+\.\d+)
      - (?P<message>(?:Apply|Destroy) complete! Resources:.*|Plan:.*|No changes\.)
      # OpenTofu boxes its errors with U+2502. The provisioner's own last line, Error: exit status 1, has
      # no box. Checkov fails a Massdriver deployment whose params hold a character outside ASCII, so the escape.
      - '\x{2502} Error: (?P<error>.+)'
    key: step/src
    label: OpenTofu step
    value: >-
      {tool: (.tool | ascii_downcase), tool_version, action: $action, status: $status,
       message: ((.error // .message) | if . then tostring | .[:280] else . end)}
      | with_entries(select(.value != null))

  - name: opentofu-changes
    detect: (OpenTofu|Terraform) has been successfully initialized
    patterns:
      - (?P<tool>OpenTofu|Terraform) has been successfully initialized
      - 'Plan: (?:(?P<imports>\d+) to import, )?(?P<add>\d+) to add, (?P<change>\d+) to change, (?P<destroy>\d+) to destroy'
      - (?P<no_changes>No changes\.)
    mode: sum
    key: changes/src
    label: OpenTofu changes
    value: >-
      {tool: (.tool | ascii_downcase), add: (.add // 0), change: (.change // 0), destroy: (.destroy // 0)}

  - name: checkov
    detect: 'Passed checks: \d+'
    patterns:
      - 'By (?:Prisma Cloud|bridgecrew\.io) \| version: (?P<tool_version>\d+\.\d+\.\d+)'
      - 'Passed checks: (?P<passed>\d+), Failed checks: (?P<failed>\d+), Skipped checks: (?P<skipped>\d+)'
    mode: sum
    key: compliance/src
    label: Checkov
    value: >-
      {tool: "checkov", tool_version, passed, failed, skipped} | with_entries(select(.value != null))
```

On a `PROVISION` with Checkov, these write:

| Key | Value |
|---|---|
| `md/v1/step/src` | `{"tool": "opentofu", "action": "apply", "status": "succeeded", "message": "Apply complete! Resources: 2 added, 1 changed, 0 destroyed."}` |
| `md/v1/changes/src` | `{"tool": "opentofu", "add": 2, "change": 1, "destroy": 0}` |
| `md/v1/compliance/src` | `{"tool": "checkov", "tool_version": "3.2.469", "passed": 36, "failed": 2, "skipped": 4}` |

OpenTofu prints its version only with `TF_LOG` set, such as the provisioner's `log_level: INFO`, so `tool_version`
is often absent. On a failed deployment, `message` is the last `Error:` line.

The Massdriver OpenTofu, Terraform, Helm, and Bicep provisioners all run Checkov, so the `checkov` rule works for
each of them.

### Helm

```yaml
rules:
  - name: helm-step
    detect: '^REVISION: \d+'
    patterns:
      - '^NAME: (?P<release>\S+)'
      - '^NAMESPACE: (?P<namespace>\S+)'
      - '^STATUS: (?P<release_status>\S+)'
      - '^REVISION: (?P<revision>\d+)'
      - 'Helm chart: .*\(version (?P<chart_version>[^)\s]+)\)'
      - '^Error: (?P<error>.+)'
    key: step/src
    label: Helm release
    value: >-
      {tool: "helm", action: $action, status: $status,
       message: ((.error // "\(.release) revision \(.revision) \(.release_status) in \(.namespace)"
                 + (if .chart_version then ", chart \(.chart_version)" else "" end)) | tostring | .[:280])}
```

```json
{"tool": "helm", "action": "apply", "status": "succeeded",
 "message": "api-prod-cache revision 4 deployed in api-prod, chart 20.6.2"}
```

The chart version comes from the provisioner's `Using OCI Helm chart` or `Using remote Helm chart` line, so a local
chart has none. A `PLAN` runs `helm upgrade --dry-run`, and its status is `pending-upgrade`.

### Bicep and ARM

```yaml
rules:
  - name: bicep-step
    detect: 'Deploying stack |"provisioningState"'
    patterns:
      - 'Deploying stack (?P<stack>[^\s.]+)'
      - '"provisioningState": "(?P<provisioning_state>\w+)"'
      - (?P<message>Stack \S+ (?:deployed successfully|deployment failed)\.)
      - '(?:ERROR|Error): (?P<error>.+)'
    key: step/src
    label: Bicep deployment
    value: >-
      {tool: "bicep", action: $action, status: $status,
       message: (.error // ([.message, (.provisioning_state | if . then "Provisioning state: \(ascii_downcase)." else . end)]
                 | map(select(. != null)) | join(" ")) | tostring | .[:280])}

  - name: bicep-what-if
    detect: 'Resource changes:'
    patterns:
      - 'Resource changes:.*?(?P<add>\d+) to create'
      - 'Resource changes:.*?(?P<change>\d+) to modify'
      - 'Resource changes:.*?(?P<destroy>\d+) to delete'
      - 'Resource changes:.*?(?P<no_op>\d+) no change'
      - '(?P<nothing>Resource changes: no change)'
    mode: sum
    key: changes/src
    label: What-if changes
    value: >-
      {tool: "bicep", add: (.add // 0), change: (.change // 0), destroy: (.destroy // 0), no_op: (.no_op // 0)}
```

| Key | Value |
|---|---|
| `md/v1/step/src` | `{"tool": "bicep", "action": "apply", "status": "succeeded", "message": "Stack api-prod-storage-src deployed successfully. Provisioning state: succeeded."}` |
| `md/v1/changes/src` | `{"tool": "bicep", "add": 1, "change": 1, "destroy": 1, "no_op": 1}` |

The Massdriver Bicep provisioner prints the stack's `provisioningState` only with `show_output: true`, and skips
what-if on a `PLAN`. The `bicep-what-if` rule reads `az deployment group what-if`, from a custom provisioner. Each
count has its own pattern because what-if lists only the counts above zero.

## Test rules on a saved log

`test log-results` reads a log file, or standard input, with the rules of a config file, and prints the entries the
automation would write. It calls no API and needs no credentials.

Save the logs of a deployment with `mass deployment logs`, then read them:

```bash
mass deployment logs <deployment-id> > deploy.log
go run ./cmd/massdriver-automations test log-results -config config.yaml -action PLAN -attr md-environment=prod deploy.log
```

Or with Docker:

```bash
docker run --rm -i -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 test log-results -action PLAN < deploy.log
```

```text
RULE              KEY                   FORMAT  LABEL             VALUE
opentofu-step     md/v1/step/src        json    OpenTofu step     {"action":"plan","message":"No changes.","status":"succeeded","tool":"opentofu","tool_version":"1.10.6"}
opentofu-changes  md/v1/changes/src     json    OpenTofu changes  {"add":0,"change":0,"destroy":0,"tool":"opentofu"}
checkov           md/v1/compliance/src  json    Checkov           {"failed":0,"passed":38,"skipped":4,"tool":"checkov","tool_version":"3.2.469"}
helm-step         -                     -       -                 skipped: detect did not match
```

| Flag | Default | |
|---|---|---|
| `-config` | `CONFIG_FILE`, or `/etc/massdriver-automations/config.yaml` | The config file. |
| `-action` | `PROVISION` | The deployment action: `PROVISION`, `PLAN`, or `DECOMMISSION`. |
| `-failed` | off | Read the log as the log of a failed deployment, so `$status` is `failed`. |
| `-attr` | none | An attribute of the deployment for `match`, such as `-attr md-project=api`. Repeat it for more. |

A rule that writes nothing says why: it does not apply to the action or the attributes, its `detect` did not
match, none of its patterns matched, or its value failed, with the jq error.

## Keys and widgets

Keys under `md/` are widget keys. The examples write `md/v1/step/<step>`, `md/v1/changes/<step>`, and
`md/v1/compliance/<step>`, which the deployment shows as widgets. The server refuses an `md/` entry when no widget
reads its key, when it is not on a deployment, or when its value does not match the key's schema. Those schemas allow
only their own fields: a step has `tool`, `tool_version`, `action`, `status`, `started_at`, `finished_at`,
`duration_s`, `exit_code`, and a `message` of up to 280 characters, so the examples put a Helm release or a Bicep
stack in the message. A refused entry is logged as `stash write failed` with the server's reason.

For keys of your own, set `results.prefix` outside `md/`, such as `results/v1`. Those entries can hold any value,
and show as labeled rows in **Deployment metadata**.

## Permissions

The service account needs, on the instances whose deployments it reads:

- `project:view`, to read the deployments and their logs
- `stash:write`, to write the entries. A deployment's entries belong to its instance.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name log-results \
  --url https://log-results.example.com/hooks/massdriver \
  --event-type deployment.completed \
  --event-type deployment.failed \
  --condition '.data.metadata.deployment_action == "PROVISION" or .data.metadata.deployment_action == "PLAN"'
```

Keep the condition the same as `actions`. To read one environment only, add `--attribute md-environment=prod`.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run log-results
```

## Limits

- The automation reads the logs before it responds, and Massdriver waits 10 seconds. Logs that take longer to read
  fail the event, and Massdriver sends it again.
- The logs of a deployment that just finished can be missing for a moment. The automation answers `500`, and
  Massdriver sends the event again. After 15 minutes it stops asking, logs `logs unavailable`, and writes nothing.
- The logs do not say which step wrote a line, so the examples name the step `src`.
- OpenTofu prints its version only with `TF_LOG` set, so `tool_version` of a step is often missing.
- The Massdriver Bicep provisioner skips what-if, and prints `provisioningState` only with `show_output: true`.
- Checkov's logs do not say whether `halt_on_failure` is on, so the examples leave out `blocking`.
- A failed stash write is logged as `stash write failed`, with the server's message, and does not fail the event.
- A line longer than 64 KiB is cut to its first 64 KiB, and a text capture to its first 1 KiB.
- Duplicate events are dropped by id for one hour, in memory.
