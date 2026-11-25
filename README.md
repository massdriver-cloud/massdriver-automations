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
| [freeze-guard](automations/freeze-guard) | `deployment.created`, `deployment.enqueued` | Aborts deployments inside a change freeze window. |
| [github-deployments](automations/github-deployments) | `deployment.*` | Shows Massdriver deployments as GitHub Deployments. |
| [proposal-chatops](automations/proposal-chatops) | `deployment.proposed`, `deployment.approved`, `deployment.rejected` | Approve or reject proposed deployments from Slack. |
| [release-notes](automations/release-notes) | `repo.published` | Posts new bundle versions, and the instances that can upgrade, to Slack and Teams. |
| [rollback-on-failure](automations/rollback-on-failure) | `deployment.failed` | Rolls an opted-in instance back to its last completed deployment when a deployment fails. |
| [router](automations/router) | configured per route | Runs HTTP requests, commands, and Massdriver API calls for the events that match configured routes. Commands use the `-scripts` image tag. |
| [sandbox-on-join](automations/sandbox-on-join) | `group_membership.*`, `deployment.completed` | Forks a sandbox environment for each new group member, and removes it when they leave. |
| [statuspage](automations/statuspage) | `alarm.*`, `alarm_state.created`, `deployment.*`, `instance.*` | Serves a public status page from alarms, instances, and deployments. |

Each automation's README has its settings and the `mass event destination create` command that sends it only
the events it uses.

## Common settings

| Variable | Default | |
|---|---|---|
| `MASSDRIVER_SIGNING_SECRET` | | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | | Your organization's identifier. Required by automations that call the API. |
| `MASSDRIVER_API_KEY` | | An `md_` access token of a service account. Required by automations that call the API. |
| `MASSDRIVER_URL` | `https://api.massdriver.cloud` | The URL of your Massdriver installation. |
| `PORT` | `8080` | |

Every automation receives events on `POST /hooks/massdriver` and answers health checks on `GET /healthz`.

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
