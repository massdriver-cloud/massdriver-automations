---
templating: mustache
---
# Autodeploy Downstream

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Events in:** https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}autodeploy-downstream.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/hooks/massdriver, from the event destination `{{params.md_metadata.name_prefix}}`
- **Rules:** {{^params.config.rules}}none; dependents with `autodeploy=true` deploy{{/params.config.rules}}
{{#params.config.rules}}
  - {{name}}: {{#action}}`{{action}}`{{/action}}{{^action}}`deploy`{{/action}}
{{/params.config.rules}}

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-autodeploy-downstream')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Lines from the `files` stream come from the helper that writes the settings to disk before the automation starts.

## A downstream instance didn't deploy

When an instance finishes a provision, each instance that depends on it and is opted in deploys with its saved params.

- **Not opted in:** it needs `autodeploy=true`, or the attributes a rule's `downstream` lists. Set them on its component, environment or project.
- **Not `PROVISIONED`:** it is skipped, so the automation never creates something for the first time.
- **Not a dependent:** one of its dependencies must resolve to a resource the upstream makes, by a link, an environment default or a remote reference.
- **The rule's action is `propose`:** a person approves the deploy in Massdriver.
- **The deploy failed to start:** the automation answers Massdriver before it deploys, so a failure is only in the logs. It is not retried.

## Deploys keep cascading

Each downstream deploy that completes sends its own event, so changes move through the graph one level at a time. To stop it, remove the attribute from the instances you want left alone, or change the rule's action to `propose`.

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
dig +short {{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}autodeploy-downstream.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}autodeploy-downstream.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`dig` returns nothing:** the DNS record is made only when **Create a DNS record** is on and the host is in the cluster's zone{{#dependencies.ecs_cluster.zone}} (`{{dependencies.ecs_cluster.zone.domain}}`){{/dependencies.ecs_cluster.zone}}. Otherwise point the name at the cluster's load balancer yourself.
- **TLS error:** the cluster's HTTPS listener has no certificate for the host. A host outside the cluster's zone needs its own certificate on the listener.
- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`200`:** the automation is up. Look for the event in the logs.
