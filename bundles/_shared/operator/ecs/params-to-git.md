---
templating: mustache
---
# Params to Git

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Settings:**

```yaml
{{{params.config}}}
```

## An instance's params are not in the repository

Each completed `PROVISION` or `DECOMMISSION` deployment writes `<root>/<project>/<environment>/<component>/params.json` to the repository of the first mapping whose `match` fits the deployment's attributes, or to `defaults`.

- **Nothing is written for any instance:** the automation is down, or its event destination is gone. See **Read the logs**.
- **One instance is missing:** a `PLAN`, failed or aborted deployment changes nothing, a mapping with `skip: true` matched it, or its params were the same as the file. `params committed` (`params written` with the `directory` strategy), `params deleted` and `pull request opened` in the logs carry the instance; `sync failed` carries the reason.
- **A pull request was opened instead of a commit:** the matching mapping uses `strategy: pull_request`.

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-params-to-git')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

## Errors

- **`sync failed` with `401`, `403` or `Resource not accessible`:** the `GITHUB_TOKEN` secret needs `Contents: write`, and `Pull requests: write` for `pull_request` (fine-grained), or `repo` (classic), on the repository.
- **`branch busy`:** another write to the branch took longer than the request. Massdriver sends the event again.
- **`reviewer request failed`:** the pull request opened, but a reviewer in `pull_request.reviewers` is not a collaborator or the token can't request teams.
- **`forbidden` from Massdriver:** the service account needs view access to the instance's project.
- **`401` or `unauthorized` from Massdriver:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with `unknown automation "params-to-git"`:** the image tag is a release without params-to-git. Use `latest` until a release has it.
- **The log stops right after start with `invalid config`:** an unknown key, a mapping without `name` or `match`, or two mappings with one name. Fix the setting and deploy.

## Check that it answers

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}params-to-git.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up.
