# massdriver-scorecards

Runs [scorecards](../../automations/scorecards) on a Kubernetes cluster. Scores instances against scorecards of jq checks over stash results and the API, rolls the scores up to environments and projects, and writes the facts they use: delivery, reliability, currency, pending changes, parity, and ownership.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run scorecards`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-scorecards-ecs](../massdriver-scorecards-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.
