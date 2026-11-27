# rollback-on-failure

When a `PROVISION` deployment of an instance with the attribute `autorollback=true` fails, roll the instance back
to its most recent `COMPLETED` `PROVISION` deployment.

The rollback has two steps:

1. `rollbackDeployment` proposes a new `PROVISION` deployment with the bundle version, params, and dependency
   connections of the completed deployment.
2. `approveDeployment` approves the proposal, so it runs.

The automation skips the event when:

- the failed deployment is itself a rollback. A failed rollback needs a person, so the automation posts a notice
  and stops.
- a deployment of the instance is `PENDING`, `APPROVED`, or `RUNNING`.
- the instance has no `COMPLETED` `PROVISION` deployment.
- a `PROVISION` that started after the failed one has completed.

When the environment has separation of duty turned on, the service account cannot approve its own proposal. The
proposal stays `PROPOSED` for a person to approve, and the notice says so.

## Opt in

Attribute keys must match `^[A-Za-z_][A-Za-z0-9_]{0,63}$`, and a key must be a custom attribute before you set it.
Create a component-scope custom attribute with the key `autorollback` and the values `true` and `false`. Set
**Required** to off, so that components without it stay valid. Then set `autorollback=true` on each component to
roll back. The attribute applies to the instances of the component in every environment.

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
| `attribute.key` | `autorollback` | The attribute key that opts an instance in. |
| `attribute.value` | `true` | The attribute value that opts an instance in. |
| `slack.channel` | | The channel for notices, with `SLACK_BOT_TOKEN`. Without a channel or a webhook, notices go to the logs only. |
| `massdriver.app_url` | `https://app.massdriver.cloud` | The Massdriver UI, for the links in notices. |
| `results.prefix` | `md/v1` | The start of the [result](#results-in-the-stash) keys. |

With the bot token, the result of each rollback, completed or failed, is a reply in the thread of its notice.

The service account needs these permissions on the opted-in instances:

- `instance:propose`, for `rollbackDeployment`
- `instance:deploy`, for `approveDeployment`
- `project:view` on their projects, to read the deployment history

## Results in the stash

On each rollback deployment it creates, the automation writes:

| Key | Stash | Label | Value |
|---|---|---|---|
| `md/v1/step/rollback` | The rollback deployment | Rollback | `{"tool": "rollback-on-failure", "status": "succeeded", "finished_at": "...", "message": "Rolls back failed deployment <id> to deployment <id> (v1.2.3)"}`. The message says when the rollback waits for approval. |

The keys become widgets once the server's widget catalog has them. Until then, the server rejects an `md/` key that is not in the catalog. Set `results.prefix` to a prefix outside `md/`, such as `results/v1`, to see each result as a metadata row with its label meanwhile. A failed write is logged at `debug` and never fails the event, so Massdriver does not send it again.

The service account needs `stash:write` on the instances.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name rollback-on-failure \
  --url https://rollback.example.com/hooks/massdriver \
  --event-type deployment.failed \
  --event-type deployment.completed \
  --condition '.data.metadata.deployment_action == "PROVISION" and .data.attributes["autorollback"] == "true" and (.type == "deployment.failed" or ((.data.entity.message // "") | startswith("Rollback to deployment ")))'
```

The condition drops `PLAN` and `DECOMMISSION` deployments, instances that did not opt in, and completed
deployments that are not rollbacks, so Massdriver marks them `SKIPPED` and does not send them. A completed rollback
is the reply in its notice's thread. If you change `attribute.key` or `attribute.value`, change the condition to
match. To act on one environment only, add
`--attribute md-environment=prod`.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e SLACK_BOT_TOKEN=xoxb-... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run rollback-on-failure
```

## Limits

- Duplicate events are dropped by id for one hour, in memory. Run one replica.
- The rollback restores the bundle version, params, and dependency connections of the completed deployment. It
  does not restore data, or changes made outside Massdriver.
- The approval pins the instance to the bundle version of the completed deployment and saves its params on the
  instance. To move forward again, set the instance's version and params.
- When the API call fails, the automation responds `500` and Massdriver retries the event. A retry approves the
  proposal from the earlier attempt instead of proposing a second one.
