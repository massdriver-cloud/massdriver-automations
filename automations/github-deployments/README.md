# github-deployments

Show Massdriver deployments as [GitHub Deployments](https://docs.github.com/en/rest/deployments/deployments) in the
repository that holds the code of an instance.

The repository comes from the `github_repo` attribute, in the form `owner/name`. Massdriver attributes are set on
projects, environments, and components, and every instance below them gets them. Declare `github_repo` as a
custom attribute at the component scope, with one value for each repository, and set it on each component.

For each Massdriver deployment, the automation creates one GitHub deployment and adds a status at each step:

| Massdriver event | GitHub deployment status |
|---|---|
| `deployment.started` | `in_progress` |
| `deployment.completed` (`PROVISION`) | `success` |
| `deployment.completed` (`DECOMMISSION`) | `inactive` |
| `deployment.failed` | `failure` |
| `deployment.aborted` | `error` |

`PLAN` deployments are skipped.

- The GitHub environment is the Massdriver environment, such as `prod`. Set `github.environment_name` to
  `project-environment` to use `api-prod` instead.
- The ref is the value of the `github_ref` attribute. Without it, the ref is the default branch of the repository.
- The GitHub deployment has the task `massdriver` and a payload with `massdriver_deployment_id`, `instance`,
  `bundle`, and `action`.
- Each status links to the deployment in the instance's history in Massdriver.
- GitHub does not merge the default branch into the ref and does not wait for commit checks, because the
  deployment already ran.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `GITHUB_TOKEN` | Required, or the variable that `github.token_env` names. A GitHub token with the `Deployments: write` and `Metadata: read` repository permissions (fine-grained), or the `repo_deployment` scope (classic). |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key stops the automation at start.

| Key | Default | |
|---|---|---|
| `github.api_url` | `https://api.github.com` | For GitHub Enterprise Server, `https://<host>/api/v3`. |
| `github.token_env` | `GITHUB_TOKEN` | The variable that holds the GitHub token. |
| `github.repo_attribute` | `github_repo` | The attribute that names the repository. |
| `github.ref_attribute` | `github_ref` | The attribute that names the ref. |
| `github.environment_name` | `environment` | `environment` or `project-environment`. |
| `massdriver.app_url` | `https://app.massdriver.cloud` | The Massdriver UI, for the status links. |

Every key is optional. The automation does not call the Massdriver API.

## Event destination

```bash
mass event destination create \
  --name github-deployments \
  --url https://automations.example.com/hooks/massdriver \
  --event-type deployment.started \
  --event-type deployment.completed \
  --event-type deployment.failed \
  --event-type deployment.aborted \
  --condition '.data.metadata.deployment_action != "PLAN" and (.data.attributes.github_repo // "") != ""'
```

If you change `github.repo_attribute`, change `github_repo` in the condition too.

Copy the signing secret from the destination into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e GITHUB_TOKEN=github_pat_... \
  massdrivercloud/massdriver-automations:latest run github-deployments
```

## Limits

- The automation handles one event at a time, so a `started` and a `completed` event that arrive together make
  one GitHub deployment.
- It remembers the GitHub deployment of each Massdriver deployment in memory. After a restart, it finds the
  deployment in the 100 newest `massdriver` deployments of the environment. Run one replica.
- A `started` event that arrives after the deployment ended adds no status.
- A GitHub error returns `500`, so Massdriver sends the event again.
- Custom attribute keys allow only letters, digits, and underscores, so the keys use underscores. A custom attribute has a fixed list of values. Add each repository and ref to that list.
- GitHub marks older successful deployments in the same environment as inactive. Two components that use one
  repository and one environment replace each other's active deployment. Use separate repositories, or set
  `github.environment_name: project-environment` when the components are in different projects.
- Authentication is by token. GitHub App authentication is not supported.
