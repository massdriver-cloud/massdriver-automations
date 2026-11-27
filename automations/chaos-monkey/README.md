# chaos-monkey

On a schedule, picks instances that match a set of attributes at random and sends each one a deployment with
params it made up. Then posts to Slack what it sent, and how each deployment ended.

Each run picks one strategy per instance, by weight, from those that fit the instance's bundle:

| Strategy | Params |
|---|---|
| `preset` | One preset of the bundle (an entry of its params schema's `examples`), merged over the instance's params. |
| `merge` | Two or more presets, in random order, merged over each other and the instance's params. Needs two presets. |
| `mutate` | The instance's params, with random fields set to new values from their schema: another `enum` or `oneOf` value, a number within `minimum` and `maximum`, a flipped boolean, a random string within `minLength` and `maxLength`, or an item added to, removed from, or shuffled in an array. Fields that cannot change any more (`$md.immutable`) stay as they are, and integers keep to `multipleOf`. |
| `gibberish` | The instance's params, with up to `gibberish.max_fields` random fields set to values of the wrong type, out of range, very long, or nonsense. It garbles fields that cannot change any more too. |

Presets merge object by object, and anything else, such as an array, replaces what was there. `mutate` does not
follow `pattern` or `format`, so some of its values fail validation too.

The mode says what the deployment is:

- `propose` (default): a proposal that waits for a person to approve or reject it. Nothing changes until then.
- `plan`: a `PLAN` deployment, which shows what would change and changes nothing.
- `deploy`: a `PROVISION` deployment that runs now.

When Massdriver rejects the params, the automation posts the validation messages. When it accepts gibberish, the
post says so. Each deployment's message starts with `chaos-monkey: `, followed by the strategy and what it did,
such as `chaos-monkey: merge: presets "Small" + "Large"`, and ends with a `signature`, such as `Hey, thanks for monkeyin' around. 🐒🤙🏼🏄‍♂️`. When that deployment completes, fails, is aborted, or, as a
proposal, is rejected, the automation posts the outcome.

The automation skips an instance when a deployment of it is `PENDING`, `APPROVED`, or `RUNNING`, and, in `propose`
mode, when a proposal of it is waiting. It moves on to the next instance until it has hit `instances` of them or run
out.

## Opt in

Point `match` at instances you are willing to break. It is required, so the automation never targets every instance
in the organization. A custom attribute such as `chaos=true` on the components or environments to target, together
with `md-environment`, keeps it to the places you choose. Attribute keys must match
`^[A-Za-z_][A-Za-z0-9_]{0,63}$`, and a key must be a custom attribute before you set it.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `CHAOS_MONKEY_RUN_TOKEN` | Serves `POST /chaos-monkey/run`. Callers send it as `Authorization: Bearer <token>`. |
| `SLACK_BOT_TOKEN` | The bot token of the [Slack app](../../README.md#slack). Posts to the channel in the config. |
| `SLACK_WEBHOOK_URL` | An incoming webhook, when there is no Slack app. Posts every message to its one channel. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

| Key | Default | |
|---|---|---|
| `match` | | Required. Effective attributes of the instances to target. A list matches any of its values. |
| `schedule` | | Standard cron, or `@every 30m`. Empty runs only from `POST /chaos-monkey/run`. |
| `timezone` | `UTC` | An IANA zone for the schedule. |
| `mode` | `propose` | `propose`, `plan`, or `deploy`. |
| `status` | `[PROVISIONED]` | Instance statuses to target: `INITIALIZED`, `PROVISIONED`, `FAILED`, `DECOMMISSIONED`. |
| `instances` | `1` | Instances to hit in each run. |
| `strategies` | `1` each | The weight of `preset`, `merge`, `mutate`, and `gibberish`. `0` turns one off. |
| `mutate.rate` | `0.3` | The chance that `mutate` changes each field. It always changes at least one. |
| `gibberish.max_fields` | `3` | The most fields that `gibberish` garbles. |
| `seed` | | A fixed seed, so a restart repeats the same sequence of picks. |
| `signature` | ten built-in sign-offs | Ends each deployment message and its Slack posts. A string, or a list to pick from at random for each deployment. `null` or `""` turns it off. |
| `slack.channel` | | The channel for posts, with `SLACK_BOT_TOKEN`. Without a channel or a webhook, posts go to the logs only. |
| `massdriver.app_url` | `https://app.massdriver.cloud` | The Massdriver UI, for the links in posts. |
| `results.prefix` | `md/v1` | The start of the [result](#results-in-the-stash) keys. |

### Run now

With `CHAOS_MONKEY_RUN_TOKEN` set:

```bash
curl -X POST -H "Authorization: Bearer $CHAOS_MONKEY_RUN_TOKEN" \
  "https://chaos.example.com/chaos-monkey/run?strategy=gibberish"
```

`strategy` is optional and forces one strategy for the run. An instance whose bundle has too few presets for
`preset` or `merge` is skipped.

The service account needs these permissions on the matching instances:

- `project:view` on their projects, to list and read them
- `instance:propose`, for `propose` mode
- `instance:deploy`, for `plan` and `deploy` modes

## Results in the stash

On each deployment it creates or proposes, the automation writes:

| Key | Stash | Label | Value |
|---|---|---|---|
| `md/v1/step/chaos-monkey` | The deployment | Chaos monkey | `{"tool": "chaos-monkey", "status": "succeeded", "finished_at": "...", "message": "<strategy>: <what it changed>"}` |

The keys become widgets once the server's widget catalog has them. Until then, the server rejects an `md/` key that is not in the catalog. Set `results.prefix` to a prefix outside `md/`, such as `results/v1`, to see each result as a metadata row with its label meanwhile. A failed write is logged at `debug` and never fails the event, so Massdriver does not send it again.

The service account needs `stash:write` on the instances.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name chaos-monkey \
  --url https://chaos.example.com/hooks/massdriver \
  --event-type deployment.completed \
  --event-type deployment.failed \
  --event-type deployment.aborted \
  --event-type deployment.rejected \
  --condition '(.data.entity.message // "") | startswith("chaos-monkey: ")'
```

The condition drops every deployment the automation did not create. Without the event destination, the automation
still runs; it only stops posting outcomes.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e SLACK_BOT_TOKEN=xoxb-... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run chaos-monkey
```

## Limits

- `propose` and `plan` change nothing until someone approves, but `deploy` changes real infrastructure, and a merged
  or mutated preset can replace or destroy resources. Match only instances you can rebuild.
- Each run starts from the instance's current params, so the chaos builds up: a `PROVISION` that runs, in `deploy`
  mode or after an approval, leaves its params on the instance for the next run. To go back, deploy the params you
  want, or roll back to a completed deployment.
- Runs do not overlap. A run that starts while another is going waits for it.
- Duplicate events are dropped by id for one hour, in memory. Run one replica.
