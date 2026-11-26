# massdriver-autodeploy-downstream

Runs [autodeploy-downstream](../../automations/autodeploy-downstream) on a Kubernetes cluster. Deploys the dependents of an instance after it deploys. Each rule selects dependents by their attributes. The first rule that matches a dependent applies.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run autodeploy-downstream`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-autodeploy-downstream-ecs](../massdriver-autodeploy-downstream-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.
