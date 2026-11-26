# massdriver-cost-explorer-ecs

Runs [cost-explorer](../../automations/cost-explorer) on Amazon ECS with Fargate. Serves a dashboard of the cloud costs of every instance. Group and filter by any instance attribute, and see the daily trend. Set the `COST_EXPLORER_PASSWORD` secret, or make it public.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run cost-explorer`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-cost-explorer](../massdriver-cost-explorer). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

It produces a `website` resource with the URL to open: the cost dashboard. The type is an example in [examples/resource-types/website](../../examples/resource-types/website). Publish it to your organization before you publish the bundle.

## Secrets

| Secret | |
|---|---|
| `COST_EXPLORER_PASSWORD` | The basic auth password. The user is the `username` in the settings, `massdriver` by default. |
