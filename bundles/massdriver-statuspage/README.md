# massdriver-statuspage

Runs [statuspage](../../automations/statuspage) on a Kubernetes cluster. Serves a public status page from the alarms, instances, and deployments of the environments on its pages.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run statuspage`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-statuspage-ecs](../massdriver-statuspage-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

It produces a `website` resource with the URL to open: the status page. The type is an example in [examples/resource-types/website](../../examples/resource-types/website). Publish it to your organization before you publish the bundle.
