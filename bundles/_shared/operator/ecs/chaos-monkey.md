---
templating: mustache
---
# Chaos Monkey

- **Mode:** {{#params.config.mode}}`{{params.config.mode}}`{{/params.config.mode}}{{^params.config.mode}}`propose`{{/params.config.mode}} (`plan` changes nothing; `propose` waits for a person; `deploy` changes things now)
- **Schedule:** {{#params.config.schedule}}`{{params.config.schedule}}` ({{#params.config.timezone}}{{params.config.timezone}}{{/params.config.timezone}}{{^params.config.timezone}}UTC{{/params.config.timezone}}){{/params.config.schedule}}{{^params.config.schedule}}only on request{{/params.config.schedule}}
- **Targets:** instances whose attributes match {{#params.config.match}}`{{.}}`{{/params.config.match}}
- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`

## Stop it now

Turn the automation off without losing its settings: set **Mode** to `plan`, or clear **Schedule**, and deploy. To stop it outright, decommission this instance; nothing else depends on it.

## What did it just do?

Every deployment it creates has a message starting with `chaos-monkey: `, naming the strategy and the params it changed. In Massdriver, open the instance it hit and read the deployment's message and plan. Its own log has each run:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-chaos-monkey')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 2h --follow
```

## It hits something it shouldn't

Its targets come only from **Match**. Narrow it, for example to `md-environment` = `dev` plus a custom attribute such as `chaos` = `true` on the components you're willing to break, and deploy.
