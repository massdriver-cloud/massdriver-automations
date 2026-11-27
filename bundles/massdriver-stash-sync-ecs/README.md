# massdriver-stash-sync-ecs

Runs [stash-sync](../../automations/stash-sync) on Amazon ECS with Fargate. Copies CloudWatch and Prometheus metrics into Massdriver stash entries on an interval, so widgets show health, usage, traffic, and banners. In simulate mode it writes changing values from a scenario, for demos. CloudWatch rules and presets use a task role that can read metrics and alarms, unless the `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` secrets are set.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run stash-sync`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Kubernetes is [massdriver-stash-sync](../massdriver-stash-sync). See [Amazon ECS](../README.md#amazon-ecs) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `AWS_ACCESS_KEY_ID` | AWS access key ID. |
| `AWS_SECRET_ACCESS_KEY` | AWS secret access key. |
| `PROMETHEUS_TOKEN` | A bearer token for the Prometheus HTTP API, for `prometheus.token_env: PROMETHEUS_TOKEN`. |
| `PROMETHEUS_PASSWORD` | The basic auth password of `prometheus.username`, for `prometheus.password_env: PROMETHEUS_PASSWORD`. |
