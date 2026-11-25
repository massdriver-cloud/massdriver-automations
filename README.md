# Massdriver automations

Services that act on [Massdriver events](https://docs.massdriver.cloud/platform-operations/event-destinations).
Each automation receives events from an HTTP event destination, verifies the
[Standard Webhooks](https://www.standardwebhooks.com) signature, and calls the Massdriver API or another service.

All automations ship in one image. Run one automation per container:

```bash
docker run -p 8080:8080 \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  massdrivercloud/massdriver-automations:latest run autodeploy-downstream
```

`AUTOMATION=<name>` works in place of `run <name>`. `list` prints every automation.

| Automation | Events | Does |
|---|---|---|
| [audit-archiver](automations/audit-archiver) | every event | Archives every event, unchanged, in S3, Splunk, or Datadog. |
| [autodeploy-downstream](automations/autodeploy-downstream) | `deployment.completed` | Deploys opted-in dependents when an instance deploys. |
| [bundle-scanner](automations/bundle-scanner) | `repo.published` | Scans each new bundle version with Trivy and Checkov. Uses the `-scanner` image tag. |
| [drift](automations/drift) | `deployment.completed`, `deployment.failed`, `deployment.aborted` | Plans instances on a schedule, alerts Slack about drift, and can re-apply the deployed configuration. |
| [freeze-guard](automations/freeze-guard) | `group.created`, `group.updated`, `group.deleted` | Adds deny policies for deployment actions to groups during change freeze windows. |
| [github-deployments](automations/github-deployments) | `deployment.*` | Shows Massdriver deployments as GitHub Deployments. |
| [proposal-chatops](automations/proposal-chatops) | `deployment.proposed`, `deployment.approved`, `deployment.rejected` | Approve or reject proposed deployments from Slack. |
| [release-notes](automations/release-notes) | `repo.published` | Posts new bundle versions, and the instances that can upgrade, to Slack and Teams. |
| [rollback-on-failure](automations/rollback-on-failure) | `deployment.failed` | Rolls an opted-in instance back to its last completed deployment when a deployment fails. |
| [router](automations/router) | configured per route | Runs HTTP requests, commands, and Massdriver API calls for the events that match configured routes. Commands use the `-scripts` image tag. |
| [sandbox-on-join](automations/sandbox-on-join) | `group_membership.*`, `deployment.completed` | Forks a sandbox environment for each new group member, and removes it when they leave. |
| [statuspage](automations/statuspage) | `alarm.*`, `alarm_state.created`, `deployment.*`, `instance.*` | Serves a public status page from alarms, instances, and deployments. |

Each automation's README has its settings and the `mass event destination create` command that sends it only
the events it uses.

## Deploy

Two Massdriver bundles run any set of the automations, with one event destination for each:

| Bundle | Runs on |
|---|---|
| [massdriver-automations](bundle) | A Kubernetes cluster, with an ingress controller. |
| [massdriver-automations-ecs](bundle-ecs) | Amazon ECS with Fargate, behind an Application Load Balancer. Its cluster type is an example in [examples/resource-types/aws-ecs-cluster](examples/resource-types/aws-ecs-cluster). |

## Common settings

| Variable | Default | |
|---|---|---|
| `MASSDRIVER_SIGNING_SECRET` | | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | | Your organization's identifier. Required by automations that call the API. |
| `MASSDRIVER_API_KEY` | | An `md_` access token of a service account. Required by automations that call the API. |
| `MASSDRIVER_URL` | `https://api.massdriver.cloud` | The URL of your Massdriver installation. |
| `PORT` | `8080` | |

Every automation receives events on `POST /hooks/massdriver` and answers health checks on `GET /healthz`.

## Run locally

Run an automation on your computer, and give Massdriver a public URL with a tunnel.

### 1. Start the automation

From a clone of this repository:

```bash
export MASSDRIVER_ORGANIZATION_ID=acme
export MASSDRIVER_API_KEY=md_...
export MASSDRIVER_SIGNING_SECRET=whsec_...   # set after step 3; any whsec_ value works until then
go run ./cmd/massdriver-automations run autodeploy-downstream
```

Or with Docker:

```bash
docker run --rm -p 8080:8080 \
  -e MASSDRIVER_ORGANIZATION_ID -e MASSDRIVER_API_KEY -e MASSDRIVER_SIGNING_SECRET \
  massdrivercloud/massdriver-automations:latest run autodeploy-downstream
```

The automation listens on port 8080. Set `PORT` to change it.

### 2. Open a tunnel

Each of these prints a public `https://` URL that forwards to port 8080.

**ngrok.** Install [ngrok](https://ngrok.com/download) and add your auth token.

```bash
ngrok http 8080
```

The `Forwarding` line shows the URL. The inspector at `http://localhost:4040` shows each request and response, and
can replay a request.

**Cloudflare Tunnel.** Install
[`cloudflared`](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/). A quick
tunnel needs no account.

```bash
cloudflared tunnel --url http://localhost:8080
```

The output shows a `https://<words>.trycloudflare.com` URL.

**localhost.run.** No install; it uses SSH.

```bash
ssh -R 80:localhost:8080 nokey@localhost.run
```

**Tailscale Funnel.** On a machine in your tailnet with Funnel turned on:

```bash
tailscale funnel 8080
```

These URLs change each time the tunnel starts, unless you have a fixed domain.

### 3. Create the event destination

Use the command in the automation's README, with the tunnel URL and the `/hooks/massdriver` path:

```bash
mass event destination create \
  --name autodeploy-downstream-local \
  --url https://<tunnel-host>/hooks/massdriver \
  --event-type deployment.completed \
  --condition '.data.metadata.deployment_action == "PROVISION"'
```

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`, and restart the
automation. Click **Send test event** on the destination: the automation answers `204` to the
`event_destination.tested` event, which shows that the URL and the secret work.

When the tunnel URL changes, update the destination:

```bash
mass event destination update <id> --url https://<new-tunnel-host>/hooks/massdriver
```

Events that arrived while the tunnel was down wait as failed deliveries. Send them again with
`mass event destination redeliver <id>`.

### Send an event without Massdriver

`scripts/send-event.sh` signs a CloudEvent file the way Massdriver does and posts it. It needs `jq`, `openssl`,
`xxd`, and `curl`.

```bash
cat > event.json <<'JSON'
{"specversion":"1.0","id":"4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2","type":"deployment.completed",
 "subject":"mri:acme:api:prod:database","time":"2026-11-24T05:54:03Z",
 "data":{"entity":{"id":"b9dc3d18-19fc-429c-8d36-6753615fe17c","action":"PROVISION"},
         "attributes":{"md-instance":"api-prod-database"},
         "metadata":{"deployment_action":"PROVISION"}}}
JSON
MASSDRIVER_SIGNING_SECRET=whsec_... scripts/send-event.sh event.json http://localhost:8080/hooks/massdriver
```

It prints the status code. Each automation drops an event id it has already handled, so change `id` to send the
same event again. To copy real events, run `mass event destination preview --event-type <type> -o json`.

## Write an automation

An automation is a Go package that implements one method:

```go
type Automation interface {
	Handle(ctx context.Context, ev automation.Event) error
}
```

- `pkg/automation` verifies the signature, drops repeated event ids, and calls `Handle`. Return an error to
  respond `500`, so Massdriver sends the event again. Massdriver waits 10 seconds for a response, so start long
  work in a goroutine.
- Implement `Routes(mux *http.ServeMux)` to serve more routes, such as a callback from Slack or a web page.
- `pkg/massdriver` calls the V2 GraphQL API. Put each operation in a `.graphql` file under `queries/` and load it
  with `//go:embed`.
- Export a `Definition` and add it to [`automations/registry.go`](automations/registry.go).
- Test with `automation.Handler` and `automation.SignRequest`, against `httptest` fakes of the APIs you call.

[`autodeploy-downstream`](automations/autodeploy-downstream) is a short, complete example.

`pkg/automation` and `pkg/massdriver` are importable, so you can also build automations in your own repository.
