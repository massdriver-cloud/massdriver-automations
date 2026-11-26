# massdriver-audit-archiver

Runs [audit-archiver](../../automations/audit-archiver) on a Kubernetes cluster. Archives every event, unchanged, to disk, S3, Splunk, or Datadog. Every event goes to every sink that is on.

The bundle runs one replica of `ghcr.io/massdriver-cloud/massdriver-automations:<tag> run audit-archiver`, serves it at **Ingress > Host**, and creates a Massdriver event destination to `https://<host>/hooks/massdriver` with only the events the automation uses. **Settings** has the keys of the automation's config file.

The same automation on Amazon ECS is [massdriver-audit-archiver-ecs](../massdriver-audit-archiver-ecs). See [Kubernetes](../README.md#kubernetes) for what the bundle creates and needs.

## Secrets

| Secret | |
|---|---|
| `AWS_ACCESS_KEY_ID` | AWS access key ID. |
| `AWS_SECRET_ACCESS_KEY` | AWS secret access key. |
| `SPLUNK_HEC_TOKEN` | Splunk HEC token. |
| `DD_API_KEY` | Datadog API key. |
