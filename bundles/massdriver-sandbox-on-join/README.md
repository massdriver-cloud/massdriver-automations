# massdriver-sandbox-on-join

Runs [sandbox-on-join](../../automations/sandbox-on-join) on a Kubernetes cluster. Forks a template environment for each person who joins a group, and removes it when they leave.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run sandbox-on-join`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-sandbox-on-join-ecs](../massdriver-sandbox-on-join-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.
