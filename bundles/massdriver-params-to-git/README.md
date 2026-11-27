# massdriver-params-to-git

Runs [params-to-git](../../automations/params-to-git) on a Kubernetes cluster. Writes the params of each successful deployment to `params.json` in a GitHub repository, by push or pull request. Set the `GITHUB_TOKEN` secret.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run params-to-git`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-params-to-git-ecs](../massdriver-params-to-git-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `GITHUB_TOKEN` | Needs `Contents` write, and `Pull requests` write for the pull request strategy, or the `repo` scope. |
| `PARAMS_MASK_KEY` | A random key. With it, each masked param is a fingerprint of the secret, so a diff shows that it changed. |
