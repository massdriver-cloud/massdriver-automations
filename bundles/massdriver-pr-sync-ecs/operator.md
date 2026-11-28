---
templating: mustache
---
# Pull Requests in the Project Stash

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Stash key:** `{{#params.config.key}}{{params.config.key}}{{/params.config.key}}{{^params.config.key}}github/prs{{/params.config.key}}` in each project
- **Reads GitHub every:** {{#params.config.interval}}`{{params.config.interval}}`{{/params.config.interval}}{{^params.config.interval}}`2m`{{/params.config.interval}}
- **Projects:**
{{#params.config.projects}}
  - {{project}}: {{#repos}}`{{.}}` {{/repos}}
{{/params.config.projects}}

## A project's pull request list is empty or old

The automation writes one entry in each project's stash. Each write lasts three intervals, so the entry goes away soon after the automation stops.

- **Every project is empty:** the automation is down. See **Read the logs** and **Check that it answers**.
- **One project stopped changing:** a query for one of its repositories failed. The log line `stash entry skipped` with `repo_query_failed` names the project, and `github query failed` names the repository and the error. The automation does not write a project while one of its repositories fails, so the old list stays until it expires. See **Errors from GitHub**.
- **One project is empty and the log says `stash entry write failed`:** the server refused the write. See **Errors from Massdriver**.
- **The note on the project shows nothing:** the note must read the same key as **Stash key**.

Each cycle ends with the log line `cycle finished`, with counts for `written`, `unchanged`, `skipped`, and `failed`.

## Read what it wrote

```bash
mass alpha stash get <project>.{{#params.config.key}}{{params.config.key}}{{/params.config.key}}{{^params.config.key}}github/prs{{/params.config.key}}
```

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-pr-sync')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Lines from the `files` stream come from the helper that writes the settings to disk before the automation starts.

## A pull request is missing from the list

- **It merged more than the merged window ago:** merged pull requests stay for **Merged window** only. `0s` lists only open ones.
- **The list is full:** a project's entry holds at most **Most pull requests**, open ones first.
- **The repository has more than 50 open pull requests:** only the 50 most recently updated are read. An older one in the merge queue is left out.

## Errors from GitHub

The log line is `github query failed`, with the repository and the error.

- **`github: HTTP 401`:** the **GitHub token** secret is wrong, expired, or revoked. Set a new one and deploy.
- **`github: repository not found`, or `github:` and GitHub's own message:** the name is wrong, or the token can't read that repository. The token needs `Pull requests`, `Commit statuses`, and `Contents` read, or the `repo` scope for private repositories.
- **`github: HTTP 403`, or a `github:` message about a rate limit:** each repository costs about one point of GitHub's GraphQL limit each cycle, out of 5,000 an hour. Raise **Interval** or list fewer repositories.
- **`context deadline exceeded`:** one query took more than 30 seconds. It is tried again next cycle.
- **GitHub Enterprise Server:** set **GitHub > GraphQL URL** to `https://<host>/api/graphql`.

## Errors from Massdriver

- **`forbidden` on a write:** the service account needs `stash:write` on each project in **Projects**.
- **`401` or `unauthorized`:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **`missing required env vars: [GITHUB_TOKEN]`:** the **GitHub token** secret is not set. Set it and deploy.
- **The log stops right after start with a config error:** an unknown key, an interval under `30s`, a project listed twice, or a repository that is not `owner/name` stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.

## Deploy fails: Set Ingress > Host

The cluster has no DNS zone, so there is no default host. Set **Host** to a name the cluster's HTTPS listener has a certificate for, or connect a DNS zone to the cluster.

## Check that it answers

The automation acts on no event; the event destination `{{params.md_metadata.name_prefix}}` sends nothing and is there for its signing secret.

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}pr-sync.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up.
