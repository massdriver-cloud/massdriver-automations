---
templating: mustache
---
# Params to Git

- **Runs on:** `{{dependencies.ecs_cluster.name}}`, {{dependencies.ecs_cluster.region}}
- **Image:** `{{params.image.repository}}:{{params.image.tag}}`
- **Events in:** https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}params-to-git.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/hooks/massdriver, from the event destination `{{params.md_metadata.name_prefix}}`
- **Default repository:** `{{params.config.defaults.repo}}`, branch {{#params.config.defaults.branch}}`{{params.config.defaults.branch}}`{{/params.config.defaults.branch}}{{^params.config.defaults.branch}}`main`{{/params.config.defaults.branch}}, strategy {{#params.config.defaults.strategy}}`{{params.config.defaults.strategy}}`{{/params.config.defaults.strategy}}{{^params.config.defaults.strategy}}`push`{{/params.config.defaults.strategy}}
- **File:** `<root>/<project>/<environment>/<component>/params.json`
- **Mappings:**{{^params.config.mappings}} none{{/params.config.mappings}}
{{#params.config.mappings}}
  - {{name}}:{{#repo}} `{{repo}}`{{/repo}}{{#strategy}} `{{strategy}}`{{/strategy}}{{#skip}} skipped{{/skip}}
{{/params.config.mappings}}

## Read the logs

The service name is cut short to fit AWS limits, so look it up:

```bash
SVC=$(aws ecs list-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} \
  --query "serviceArns[?ends_with(@, '-params-to-git')] | [0]" --output text | awk -F/ '{print $NF}')
aws logs tail /ecs/$SVC --region {{dependencies.ecs_cluster.region}} --since 30m --follow
```

Lines from the `files` stream come from the helper that writes the settings to disk before the automation starts. Lines about an event name the `instance`, the `deployment_id`, and the `mapping`.

## A deployment did not change the file

Look for the instance in the logs. A line that worked says `params committed`, `params deleted`, `pull request opened`, or `params printed`.

- **No line for the instance:** the automation skipped the event on purpose. Only completed `PROVISION` and `DECOMMISSION` deployments sync. It also skips when the mapping has **Skip** on, when **On decommission** is `keep`, when the instance is not deployed, when a newer deployment of the instance already ran, when the instance was provisioned again before its decommission event, and when the params are the same as the file.
- **`params printed`:** the mapping's strategy is `stdout`. The file is in the log, not in GitHub.
- **`sync failed`:** GitHub refused the write. See **Errors from GitHub**.
- **`event handler failed`:** a retry can fix it, so Massdriver sends the event again. See **Errors from GitHub** and **Errors from Massdriver**.
- **`commit message failed`, `pull request branch failed`, or `pull request title failed`:** the `\(expr)` text in that setting gave an error or an empty value. A pull request branch that is the same as **Branch** also fails. Fix the setting and deploy this automation, then deploy the instance again.
- **`params render failed`:** the instance's params could not be read as JSON. It is not tried again. The log has the error.

## Errors from GitHub

The error looks like `github: <step>: HTTP <status>: <message>`, such as `github: put file: HTTP 403: ...`.

These are not tried again. Fix the cause, then deploy the instance again to write its file:

- **`HTTP 401`:** the **GitHub token** secret is wrong, expired, or revoked. Set a new one and deploy.
- **`HTTP 403` that is not a rate limit:** the token can't write. It needs `Contents` write, and `Pull requests` write for the `pull_request` strategy, or the `repo` scope.
- **`get ref: HTTP 404: branch not found`:** the mapping's **Branch** doesn't exist in the repository.
- **`HTTP 404` on another step:** the repository is wrong, or the token can't see it.
- **Another `HTTP 4xx`:** the message says why.

These are tried again: Massdriver sends the event again and the log says `event handler failed`.

- **`HTTP 409`:** the branch moved during the write. The retry reads the branch again.
- **`HTTP 429`, or `HTTP 403` with no rate limit left:** GitHub's rate limit. One token serves every mapping.
- **`HTTP 5xx` or a network error:** GitHub is having trouble.
- **`timed out waiting for another write to the branch`:** another deployment was writing the same branch for the whole 8 seconds the automation has for an event.

## Pull requests

- **A pull request was not opened:** the log says `sync failed` with the step `create pull`. See **Errors from GitHub**.
- **`reviewer request failed`:** the pull request is open, but the reviewers were not asked. The login or team is wrong, or the token can't ask them. Ask on the pull request yourself. It is not tried again.
- **Commits pushed by hand to the `massdriver/params/...` branch went away:** while no pull request is open, the automation resets that branch to **Branch** before it writes. Edit the files on **Branch**, not on that branch.
- **A file edited on the open pull request was overwritten:** each deployment writes the whole file again.
- **A pull request was closed without merging:** the next change starts the branch again from **Branch** and opens a new pull request.
- **One pull request for each deployment:** **Rolling** is off. Each branch ends with the first 8 characters of the deployment ID.
- Merging a pull request records the params and deploys nothing.

## A secret shows in the file

The file hides a param only when the bundle's params schema marks it with `$md.sensitive: true`, `format: password`, or `writeOnly: true`. Change the secret. Then mark the field in the bundle, publish it, and deploy the instance.

- **The value is `[SENSITIVE]`:** this is the mask. **Sensitive params** set to `omit` drops the key.
- **The value is `[SENSITIVE:<fingerprint>]`:** the **PARAMS_MASK_KEY** secret is set. The fingerprint changes when the secret changes.
- **Many files changed with only new fingerprints:** **PARAMS_MASK_KEY** was set, removed, or changed. Each file gets one such commit on its instance's next deployment. Keep the key the same.

## Errors from Massdriver

The automation reads each instance's last completed `PROVISION` deployment, its params, and its bundle's params schema. These errors are tried again.

- **`forbidden`:** the service account needs view access to the project of each instance it syncs.
- **`401` or `unauthorized`:** the service account's key is gone. Give the **service_account** resource a new key and deploy.

## The task won't start or keeps restarting

```bash
aws ecs describe-services --region {{dependencies.ecs_cluster.region}} --cluster {{dependencies.ecs_cluster.name}} --services $SVC \
  --query 'services[0].events[:10].[createdAt,message]' --output text
```

- **`CannotPullContainerError`:** the image tag `{{params.image.tag}}` doesn't exist. Pick a published tag and deploy.
- **`missing required env vars: [GITHUB_TOKEN]`:** a mapping uses `push` or `pull_request` and the **GitHub token** secret is not set. Set it and deploy.
- **The log stops right after start with a config error:** a bad value, an unknown key, a mapping without a name or match, two mappings with one name, or `\(expr)` text that does not compile stops the automation. Fix the setting and deploy.
- **`Task failed ELB health checks`:** the automation answers `/healthz` on port 8080 once it is ready. The log says why it isn't.

## Deploy fails: Set Ingress > Host

The cluster has no DNS zone, so there is no default host. Set **Host** to a name the cluster's HTTPS listener has a certificate for, or connect a DNS zone to the cluster.

## Events don't arrive

Massdriver sends events to the event destination `{{params.md_metadata.name_prefix}}` (**Settings > Event Destinations**). It sends only completed deployments that are not `PLAN`. Massdriver pauses a destination when sends fail for a day; the next deploy of this instance resumes it.

Check that the automation answers:

```bash
dig +short {{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}params-to-git.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}
curl -s -o /dev/null -w '%{http_code}\n' https://{{#params.ingress.host}}{{params.ingress.host}}{{/params.ingress.host}}{{^params.ingress.host}}{{#dependencies.ecs_cluster.zone}}params-to-git.{{dependencies.ecs_cluster.zone.domain}}{{/dependencies.ecs_cluster.zone}}{{/params.ingress.host}}/healthz
```

- **`dig` returns nothing:** the DNS record is made only when **Create a DNS record** is on and the host is in the cluster's zone{{#dependencies.ecs_cluster.zone}} (`{{dependencies.ecs_cluster.zone.domain}}`){{/dependencies.ecs_cluster.zone}}. Otherwise point the name at the cluster's load balancer yourself.
- **TLS error:** the cluster's HTTPS listener has no certificate for the host. A host outside the cluster's zone needs its own certificate on the listener.
- **`503`:** no task is healthy. See **The task won't start or keeps restarting**.
- **`204`:** the automation is up. Look for the event in the logs.
- **`signature rejected` in the logs:** the task has an old signing secret. Deploy this instance.
