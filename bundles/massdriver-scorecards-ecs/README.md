# massdriver-scorecards-ecs

Runs [scorecards](../../automations/scorecards) on Amazon ECS with Fargate. Scores instances against scorecards, and writes the scores and the facts they use (delivery, reliability, currency, pending changes, parity, and ownership) to the stash. Each event and each interval computes them again from the API.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run scorecards`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-scorecards](../massdriver-scorecards). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.
