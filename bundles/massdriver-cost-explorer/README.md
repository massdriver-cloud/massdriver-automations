# massdriver-cost-explorer

Runs [cost-explorer](../../automations/cost-explorer) on a Kubernetes cluster. Serves a dashboard of the cloud costs of every instance. Group and filter by any instance attribute, and see the daily trend. Set the `COST_EXPLORER_PASSWORD` secret, or make it public.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run cost-explorer`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-cost-explorer-ecs](../massdriver-cost-explorer-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

It produces a `website` resource with the URL to open: the cost dashboard. The type is an example in [examples/resource-types/website](../../examples/resource-types/website). Publish it to your organization before you publish the bundle.

## Secrets

| Secret | |
|---|---|
| `COST_EXPLORER_PASSWORD` | The basic auth password. The user is the `username` in the settings, `massdriver` by default. |
