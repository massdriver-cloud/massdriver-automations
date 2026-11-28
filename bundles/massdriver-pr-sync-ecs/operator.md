---
templating: mustache
---
# Pull Request Sync

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Settings:**

```yaml
{{{params.config}}}
```

## A project's note lists no pull requests

Each interval, the automation reads each repository's pull requests and writes them to the `github/prs` key of each project's stash, unless **Settings > Stash key** names another.

- **No project gets the entry:** the automation is down. See **Read the logs**.
- **One project has none:** the log line `github query failed` names a repository the `GITHUB_TOKEN` secret cannot read, and `stash entry skipped` follows it for each project that lists that repository. `stash entry write failed` carries the server's reason.
- **The list is old:** an entry expires after three intervals without a write, so a list that stops changing means the automation stopped. See **The task won't start or keeps restarting**.

To list them on the project, pin the note from [Pin the note](https://docs.massdriver.cloud/onboarding/automations#pin-the-note).

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-pr-sync')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

`pr sync started` with the interval means the settings loaded.

## Errors

- **`github query failed` with `401` or `Bad credentials`:** set a new `GITHUB_TOKEN` and deploy. A fine-grained token needs `Pull requests`, `Commit statuses` and `Contents` read on every repository in the settings; a classic token needs `repo` for private repositories.
- **`stash entry write failed` with `forbidden`:** the service account needs `stash:write` on that project.
- **`401` or `unauthorized` from Massdriver:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **The log stops right after start with `unknown automation "pr-sync"`:** the image tag is a release without pr-sync. Use `latest` until a release has it.
- **The log stops right after start with `invalid config`:** the record names the key. Fix the setting and deploy.

## Check that it answers

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}pr-sync.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up.
