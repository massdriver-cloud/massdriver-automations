# massdriver-router-ecs

Runs [router](../../automations/router) on Amazon ECS with Fargate. Runs HTTP requests, commands, and Massdriver API calls for the events that match its routes. The event destination sends the event types of every route. The `ROUTER_ENV` secret is a JSON object of extra environment variables, for `env.NAME` in jq or `{{ env "NAME" }}` in Go templates.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run router`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-router](../massdriver-router). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `ROUTER_ENV` | A JSON object of environment variables for routes, such as `{"PAGERDUTY_ROUTING_KEY": "..."}`. |
