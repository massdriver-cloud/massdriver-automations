# massdriver-oncall

Runs [oncall](../../automations/oncall) on a Kubernetes cluster. Writes who is on call now, from PagerDuty or incident.io, to the on-call widget of environments on an interval. A route can also write a fixed list, to try the widget. Set the `PAGERDUTY_TOKEN` or `INCIDENT_IO_API_KEY` secret for the provider that routes use.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run oncall`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-oncall-ecs](../massdriver-oncall-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `PAGERDUTY_TOKEN` | A read-only PagerDuty REST API key. |
| `INCIDENT_IO_API_KEY` | Needs `schedules.view` and `escalation_paths.view`. |
