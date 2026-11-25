# decommission-cleanup

When Massdriver tears down or deletes infrastructure, run hooks that clean up what the IaC does not manage, such as
DNS records in another account, monitoring entries, or tickets.

A hook runs for these events:

| Event | When |
|---|---|
| `deployment.completed` | A `DECOMMISSION` deployment of an instance finished. Other actions are ignored. |
| `instance.deleted` | An instance was removed. |
| `environment.deleted` | An environment was deleted. |

Each hook lists the event types that select it, and optional attributes that the event must have. A hook is an
HTTP request or a command. Hooks run one event at a time, after the automation responds to Massdriver, in the order
of the file.

## Hooks file

```yaml
hooks:
  - name: release-dns
    events: [deployment.completed, instance.deleted]
    match:
      md-environment: prod
    timeout: 30s
    http:
      method: DELETE
      url: 'https://dns.example.com/records/{{ attr "md-instance" }}'
      headers:
        Authorization: 'Bearer {{ env "DNS_TOKEN" }}'
      body: '{"instance": {{ json (attr "md-instance") }}, "event": {{ json .id }}}'

  - name: purge-environment-bucket
    events: [environment.deleted]
    timeout: 5m
    exec:
      command: [/hooks/purge-bucket, '{{ .data.entity.identifier }}']
```

| Field | |
|---|---|
| `name` | Required. Unique. Shown in the logs. |
| `events` | Required. Event types. `instance.*` selects every event type of one entity. |
| `match` | Attributes that the event's entity must have, each with exactly this value. |
| `timeout` | Default `30s`. The automation stops the request or the command at the timeout. |
| `http.method` | Default `POST`. |
| `http.url`, `http.headers`, `http.body` | Templates. A response other than `2xx` is a failure. |
| `exec.command` | The program and its arguments, each a template. No shell runs the command. |

Set exactly one of `http` and `exec`.

### Templates

Templates use Go [`text/template`](https://pkg.go.dev/text/template). The data is the event, as Massdriver sends
it: `.id`, `.type`, `.subject`, `.time`, `.data.entity`, `.data.attributes`, and `.data.metadata`. A field that the
event does not have stops the hook with an error, and the hook does not run.

| Function | |
|---|---|
| `attr "md-instance"` | An attribute of the event's entity. Use it for keys with `-`. |
| `json .data.entity` | The value as JSON. Use it to put values in a JSON body. |
| `env "DNS_TOKEN"` | An environment variable of the automation. Use it for credentials. |

### Commands

A command gets the event as JSON on standard input, and these environment variables:

| Variable | |
|---|---|
| `MD_EVENT_ID`, `MD_EVENT_TYPE`, `MD_EVENT_SUBJECT`, `MD_EVENT_TIME` | From the event. |
| `MD_ATTR_<KEY>` | Each attribute. The key is in upper case, with `_` in place of other characters: `md-instance` is `MD_ATTR_MD_INSTANCE`. |
| `MD_METADATA_<KEY>` | Each metadata field, such as `MD_METADATA_DEPLOYMENT_ACTION`. |

A command also gets the environment of the automation, without `MASSDRIVER_SIGNING_SECRET` and
`MASSDRIVER_API_KEY`. A command that exits with a status other than `0` is a failure.

## Configuration

| Variable | Default | |
|---|---|---|
| `MASSDRIVER_SIGNING_SECRET` | | Required. The `whsec_` signing secret of the event destination. |
| `CLEANUP_HOOKS_FILE` | `/etc/decommission-cleanup/hooks.yaml` | The hooks file. The automation does not start when the file has an error. |
| `PORT` | `8080` | |

The automation does not call the Massdriver API.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name decommission-cleanup \
  --url https://cleanup.example.com/hooks/massdriver \
  --event-type deployment.completed \
  --event-type instance.deleted \
  --event-type environment.deleted \
  --condition '.type != "deployment.completed" or .data.metadata.deployment_action == "DECOMMISSION"'
```

The condition drops `PROVISION` and `PLAN` deployments, so Massdriver marks them `SKIPPED` and does not send them. To
act on one project only, add `--attribute md-project=api`.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e CLEANUP_HOOKS_FILE=/hooks/hooks.yaml \
  -e DNS_TOKEN=... \
  -v "$PWD/hooks:/hooks:ro" \
  massdrivercloud/massdriver-automations:latest run decommission-cleanup
```

The image has no shell and no tools. For `exec` hooks, use static binaries, or build an image that has the
programs the commands need:

```dockerfile
FROM massdrivercloud/massdriver-automations:latest AS automations

FROM alpine:3.22
RUN apk add --no-cache aws-cli curl
COPY --from=automations /massdriver-automations /massdriver-automations
COPY hooks/ /hooks/
USER 65532
ENTRYPOINT ["/massdriver-automations", "run", "decommission-cleanup"]
```

## Limits

- The result of each hook goes to the logs: the HTTP status and response, or the command output, up to 4 KiB.
- The automation responds before the hooks run. A failed hook is logged. Massdriver does not send the event again,
  and the automation does not retry the hook.
- The queue holds 100 events, in memory. When it is full, the automation responds `500`, and Massdriver sends the
  event again later. A restart loses the queue.
- Duplicate events are dropped by id for one hour, in memory. Run one replica.
