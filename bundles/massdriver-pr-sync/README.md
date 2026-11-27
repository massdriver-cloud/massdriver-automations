# massdriver-pr-sync

Runs [pr-sync](../../automations/pr-sync) on a Kubernetes cluster. Writes the pull requests of a project's GitHub repositories to a JSON entry in the project's stash on an interval, so a note on the project lists their review, check, and merge state. Set the `GITHUB_TOKEN` secret.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run pr-sync`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-pr-sync-ecs](../massdriver-pr-sync-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `GITHUB_TOKEN` | Needs `Pull requests`, `Commit statuses`, and `Contents` read, or the `repo` scope for private repositories. |
