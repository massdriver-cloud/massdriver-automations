# massdriver-github-deployments

Runs [github-deployments](../../automations/github-deployments) on a Kubernetes cluster. Shows Massdriver deployments as GitHub Deployments on the repository that an instance names. Set the `GITHUB_TOKEN` secret.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run github-deployments`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-github-deployments-ecs](../massdriver-github-deployments-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `GITHUB_TOKEN` | Needs `Deployments` write and `Metadata` read, or the `repo_deployment` scope. |
