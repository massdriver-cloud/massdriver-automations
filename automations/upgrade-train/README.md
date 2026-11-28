# upgrade-train

Moves each new version of a bundle through stages of instances, such as dev, then staging, then prod. A stage
deploys or proposes the version, waits a soak time with no firing alarms, and then the next stage starts. A failed
deployment or a firing alarm stops the train.

When a bundle that a train covers publishes a version (`repo.published`), the train starts a run for it:

1. The stage lists the `PROVISIONED` instances of the bundle that match its attributes.
2. An instance already on the version counts as done. An instance whose version constraint does not resolve to the
   new version, such as one pinned to `1.2.0`, is left out and named in the post. The train never changes an
   instance's version constraint.
3. Each other instance gets a `PROVISION` deployment of its current params: created now in `deploy` mode, or
   proposed for a person to approve in `propose` mode. Its message is
   `upgrade-train: <train> <bundle>@<version> stage <stage>`.
4. An instance with a deployment `PENDING`, `APPROVED`, or `RUNNING` is retried every minute, for up to an hour.
   Then the stage leaves it out and says so.
5. When every deployment of the stage has completed, the soak starts. During the soak, the train checks every
   minute that each instance of the stage is still on the version and has no firing alarms.
6. After the soak, the next stage starts. After the last stage, the train posts that it finished.

The run stops, and posts why, when a deployment of the stage fails, is aborted, or is rejected, when a deployment
cannot be created, or when an instance has a firing alarm or leaves the version during the soak. A stopped run
stays stopped until `POST /upgrade-train/run` resumes it at the stage where it stopped.

A newer version of the bundle replaces a run that is under way. Each bundle of a train has its own run.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `STATE_DIR` | A directory that keeps the runs across restarts. Without it, a restart forgets them. |
| `UPGRADE_TRAIN_TOKEN` | Serves `POST /upgrade-train/run` and `POST /upgrade-train/halt`. Callers send it as `Authorization: Bearer <token>`. |
| `SLACK_BOT_TOKEN` | The bot token of the [Slack app](../../README.md#slack). Posts to the channels in the config. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, when there is no Slack app. Posts every message to its one channel. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

| Key | Default | |
|---|---|---|
| `trains[].name` | | Required. Unique. |
| `trains[].bundles` | | Required. The bundles (OCI repo names) whose versions ride the train. |
| `trains[].include_development` | `false` | Also run development versions, such as `1.3.1-dev.20261124T055403Z`. |
| `trains[].stages[].name` | | Required. Unique in the train. |
| `trains[].stages[].match` | | Required. Effective attributes of the stage's instances. A list matches any of its values. |
| `trains[].stages[].mode` | `propose` | `deploy` or `propose`. |
| `trains[].stages[].soak` | `1h` | How long the stage waits after its deployments complete. A Go duration, such as `30m` or `24h`. |
| `trains[].slack.channel` | | Replaces `slack.channel` for this train. |
| `slack.channel` | | The channel for posts, with `SLACK_BOT_TOKEN`. Without a channel or a webhook, posts go to the logs only. |
| `massdriver.app_url` | `https://app.massdriver.cloud` | The Massdriver UI, for the links in posts. |
| `results.prefix` | `md/v1` | The start of the [result](#results-in-the-stash) keys. |

### Start, resume, or stop a run

With `UPGRADE_TRAIN_TOKEN` set:

```bash
# Start the latest version of the train's bundle, or resume a stopped run of it.
curl -X POST -H "Authorization: Bearer $UPGRADE_TRAIN_TOKEN" \
  "https://train.example.com/upgrade-train/run?train=databases&bundle=aws-rds-mysql&version=1.3.0"

# Stop every run of the train, or of one bundle with &bundle=.
curl -X POST -H "Authorization: Bearer $UPGRADE_TRAIN_TOKEN" \
  "https://train.example.com/upgrade-train/halt?train=databases"
```

`bundle` is required when the train has more than one bundle. Without `version`, the run uses the newest version
of the bundle.

The service account needs these permissions on the instances of every stage:

- `project:view` on their projects, to read the instances, and read access to the bundles' repos, to read the versions
- `instance:deploy`, for `deploy` stages
- `instance:propose`, for `propose` stages

## Results in the stash

Entries in a deployment's stash are immutable: they record what already happened, and when Massdriver sends an event again, the first value stays.

| Key | Stash | Label | Value |
|---|---|---|---|
| `md/v1/step/upgrade-train` | Each deployment a stage creates or proposes | Upgrade train | `{"tool": "upgrade-train", "status": "succeeded", "finished_at": "...", "message": "Stage dev of train db: api-db@1.3.0"}` |
| `md/v1/release` | The instance, when that deployment completes | Release | `{"version": "1.3.0", "deployed_at": "..."}` |

The keys become widgets once the server's widget catalog has them. Until then, the server rejects an `md/` key that is not in the catalog. Set `results.prefix` to a prefix outside `md/`, such as `results/v1`, to see each result as a metadata row with its label meanwhile. A failed write is logged at `debug` and never fails the event, so Massdriver does not send it again.

The service account needs `stash:write` on the instances.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name upgrade-train \
  --url https://train.example.com/hooks/massdriver \
  --event-type repo.published \
  --event-type deployment.completed \
  --event-type deployment.failed \
  --event-type deployment.aborted \
  --event-type deployment.rejected \
  --condition '.type == "repo.published" or ((.data.entity.message // "") | startswith("upgrade-train: "))'
```

The condition drops every deployment that the train did not create.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -v "$PWD/state:/var/lib/upgrade-train" \
  -e STATE_DIR=/var/lib/upgrade-train \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e SLACK_BOT_TOKEN=xoxb-... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run upgrade-train
```

## Limits

- The train deploys each instance with the params it has now. It does not change params or version constraints.
- An instance that joins a stage after the stage started waits for the next version.
- The soak watches firing alarms only. A stage with no alarms soaks on time alone.
- Duplicate events are dropped by id for one hour, in memory. Run one replica.
