# massdriver-autodeploy-downstream-ecs

Runs [autodeploy-downstream](../../automations/autodeploy-downstream) on Amazon ECS with Fargate. Deploys the dependents of an instance after it deploys. Each rule selects dependents by their attributes. The first rule that matches a dependent applies.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run autodeploy-downstream`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-autodeploy-downstream](../massdriver-autodeploy-downstream). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.
