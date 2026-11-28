# massdriver-leaderboard-ecs

Runs [leaderboard](../../automations/leaderboard) on Amazon ECS with Fargate. Ranks the authors of GitHub pull requests and writes the top players to the leaderboard widget of environments on an interval. A route can also write a fixed list, to try the widget. Set the `GITHUB_TOKEN` secret for routes that read pull requests.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run leaderboard`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-leaderboard](../massdriver-leaderboard). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `GITHUB_TOKEN` | Needs `Pull requests` and `Contents` read, or the `repo` scope for private repositories. |
