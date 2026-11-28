---
templating: mustache
---
# Audit Archiver

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Events in:** https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}audit-archiver.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/hooks/massdriver, from the event destination `{{params.md_metadata.name_prefix}}`
{{#params.config.s3.enabled}}
- **S3:** `{{params.config.s3.bucket}}`{{#params.config.s3.prefix}}, prefix `{{params.config.s3.prefix}}`{{/params.config.s3.prefix}}
{{/params.config.s3.enabled}}
{{#params.config.splunk.enabled}}
- **Splunk:** {{params.config.splunk.url}}{{#params.config.splunk.index}}, index `{{params.config.splunk.index}}`{{/params.config.splunk.index}}
{{/params.config.splunk.enabled}}
{{#params.config.datadog.enabled}}
- **Datadog:** {{params.config.datadog.site}}
{{/params.config.datadog.enabled}}
{{#params.config.disk.enabled}}
- **Disk:** `{{params.config.disk.dir}}`, lost when the task restarts
{{/params.config.disk.enabled}}

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-audit-archiver')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Lines from the `files` stream come from the helper that writes the settings to disk before the automation starts.

## Events are missing from a sink

Massdriver gets a success only after every sink accepts the event. When one fails, Massdriver sends the event again, and the retry writes only to the sinks that failed. The logs name the sink and its error.

- **Each sink has 8 seconds** to accept an event. A slower sink fails the event.
- **S3 `AccessDenied`:** without the AWS key secrets, the task's role writes to the bucket. Check the bucket name and deploy.
- **Splunk:** check the **Splunk HEC token** secret. Splunk accepts an event before it indexes it, so a `200` doesn't prove it is searchable.
- **Datadog:** check the **Datadog API key** secret and the site.
- **Bodies over 1 MB** are refused.

## Duplicate events in Splunk or Datadog

After a restart, a retry writes to every sink again. S3 writes the same key. Splunk and Datadog can hold a copy; drop it in a search with `webhook_id`.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with a config error:** an unknown key or a bad value in the settings stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.
- **`401` or `unauthorized` from Massdriver:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## Deploy fails: Set Ingress > Host

The cluster has no DNS zone, so there is no default host. Set **Host** to a name the cluster's HTTPS listener has a certificate for, or connect a DNS zone to the cluster.

## Events don't arrive

Massdriver sends events to the event destination `{{params.md_metadata.name_prefix}}` (**Settings → Event Destinations**). It only sends the event types this automation needs. Massdriver pauses a destination when sends fail for a day; the next deploy of this instance resumes it.

Check that the automation answers:

```bash
dig +short {{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}audit-archiver.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}audit-archiver.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`dig` returns nothing:** the DNS record is made only when **Create a DNS record** is on and the host is in the cluster's zone{{#dependencies.ecs_cluster.zone}} (`{{dependencies.ecs_cluster.zone.domain}}`){{/dependencies.ecs_cluster.zone}}. Otherwise point the name at the cluster's load balancer yourself.
- **TLS error:** the cluster's HTTPS listener has no certificate for the host. A host outside the cluster's zone needs its own certificate on the listener.
- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`200`:** the automation is up. Look for the event in the logs.
