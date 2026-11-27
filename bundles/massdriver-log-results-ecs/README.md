# massdriver-log-results-ecs

Runs [log-results](../../automations/log-results) on Amazon ECS with Fargate. Reads the logs of each finished deployment with the rules in its settings, and writes what they find, such as plan changes, Helm releases, and Checkov findings, to the deployment's stash as Deployment metadata.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run log-results`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-log-results](../massdriver-log-results). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.
