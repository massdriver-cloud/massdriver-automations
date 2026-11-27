# params-to-git

When a deployment completes, write the params it deployed to a GitHub repository, one file for each instance:

```text
<root>/<project>/<environment>/<component>/params.json
```

The repository becomes a record of what each instance runs, with a commit for each change. Use it to review
configuration in pull requests, to search it, or to compare environments with `diff`.

- Only completed `PROVISION` and `DECOMMISSION` deployments sync. A failed, aborted, or `PLAN` deployment
  changes nothing, so the file always holds the params of the last deployment that succeeded.
- The sync goes one way, from Massdriver to git. Merging a pull request or editing a file deploys nothing.
- The file is stable JSON: sorted keys, two-space indent, and a trailing newline. A deployment with the same params
  as the file makes no commit.
- `md_metadata`, which Massdriver adds to every deployment, is not written.
- Writes to one branch run one at a time, in the order the events arrive. An event for a deployment older than the
  instance's last completed `PROVISION` is skipped, so a late or repeated event never overwrites newer params.

## Where each instance goes

A mapping matches the attributes of the deployment event: `md-project`, `md-environment`, `md-component`, and
custom attributes. The first mapping that matches applies. An instance that no mapping matches uses `defaults`.
Each key that a mapping leaves out comes from `defaults`.

```yaml
defaults:
  repo: acme/platform
mappings:
  - name: prod-needs-review
    match:
      md-environment: prod
    strategy: pull_request
    pull_request:
      reviewers: [acme/platform-team]
  - name: ecomm-own-repo
    match:
      md-project: ecomm
    repo: acme/ecomm
    root: deploy/massdriver
  - name: skip-previews
    match:
      md-environment: [preview, sandbox]
    skip: true
```

`root` is the only part of the path you choose. The rest is fixed, so two instances never write one file and tools
can walk the tree.

To sync only the instances that a mapping matches, set `skip: true` in `defaults` and `skip: false` in the
mappings.

## Push or pull request

`push` commits to `branch`.

`pull_request` commits to a branch of its own, `massdriver/params/<project>-<environment>` by default, and opens a
pull request into `branch`. With `rolling: true`, the default, later deployments add commits to the open pull
request. After it is merged or closed, the next change starts the branch again from `branch` and opens a new pull
request. With `rolling: false`, each deployment gets its own branch and pull request. The deployment has already
run, so the pull request is a record to review, and merging it deploys nothing.

`reviewers` are asked to review each new pull request. A name with a slash, such as `acme/platform-team`, is a
team.

## Try it without GitHub

Two more strategies write nothing to GitHub and need no token. Use them to try the automation, or to check where a
mapping sends an instance and what its file holds before you point it at a repository.

`stdout` prints one block for each sync: the action, the path, the repository and branch that `push` would write,
the commit message, and the file. Secrets are masked or omitted as in the repository. Logs go to stderr, so stdout
holds only the blocks.

```yaml
defaults:
  strategy: stdout
```

```text
--- params-to-git write api/prod/database/params.json
instance: api-prod-database
deployment: 6f1c...
mapping: defaults
branch: main
message: api-prod-database: provision deployment 6f1c...
{
  "database": {
    "name": "app",
    "password": "[SENSITIVE]"
  },
  "size": "large"
}
--- end
```

A decommission prints `--- params-to-git delete <path>` and no file. `stdout` has no copy of the file, so it
prints every sync, even when the params did not change.

`directory` writes the files under `debug.directory`, in the same layout as the repository. It skips a file that
has not changed, and deletes it on decommission with `on_decommission: delete`.

```yaml
debug:
  directory: /out
defaults:
  strategy: directory
```

```bash
mkdir -p params
docker run -p 8080:8080 \
  --user "$(id -u):$(id -g)" \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -v "$PWD/params:/out" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run params-to-git
```

For `stdout`, leave out the `params` volume and `--user`, and add `> params.log` to keep the blocks in a file.
Massdriver needs a public URL to send events to your computer. See [Run locally](../../README.md#run-locally).

A mapping can set its own strategy, so one project can print while the others push:

```yaml
defaults:
  repo: acme/platform
mappings:
  - name: try-lab
    match:
      md-project: lab
    strategy: stdout
```

The GitHub token is required only when `defaults` or a mapping that is not skipped uses `push` or `pull_request`.
`repo` is optional with `stdout` and `directory`. `stdout` prints it when it is set.

## Secrets

A param is sensitive when its field in the bundle's params schema has `$md.sensitive: true`, `format: password`,
or `writeOnly: true`. Fields in `dependencies`, `oneOf`, `anyOf`, `allOf`, `if`/`then`/`else`, and local `$ref`s
count. The value of a sensitive param is never written:

- `mask` keeps the key with the value `[SENSITIVE]`. Set `PARAMS_MASK_KEY` to write `[SENSITIVE:<fingerprint>]`,
  an HMAC of the value, the instance, and the field, so a diff shows that a secret changed. Keep the key secret:
  without it, a fingerprint cannot be checked against a guessed value. Changing the key changes every
  fingerprint, and each file then gets one commit.
- `omit` drops the key.

## Decommission

A decommission arrives as `deployment.completed` with `deployment_action` `DECOMMISSION`. `on_decommission: delete`,
the default, deletes the instance's `params.json`, by the mapping's strategy. `keep` leaves the file. If the
instance is provisioned again before the event arrives, the file stays.

## Commits

With `commit.author: actor`, the default, the author is the person or service account that ran the deployment,
with the email `commit.email`, because Massdriver events do not carry one. With `bot`, the author is the account of
the GitHub token. The committer is always the account of the token.

`commit.message`, `pull_request.branch`, and `pull_request.title` are text with `\(expr)` interpolations of jq
over the event, as in [router](../router). The default message is:

```text
api-prod-database: provision deployment 6f1c...
```

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `GITHUB_TOKEN` | Required with `push` or `pull_request`, or the variable that `github.token_env` names. A GitHub token with the `Contents: write` repository permission, and `Pull requests: write` for the `pull_request` strategy (fine-grained), or the `repo` scope (classic). |
| `PARAMS_MASK_KEY` | Optional. A random key that turns each masked value into a fingerprint. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

| Key | Default | |
|---|---|---|
| `github.api_url` | `https://api.github.com` | For GitHub Enterprise Server, `https://<host>/api/v3`. |
| `github.token_env` | `GITHUB_TOKEN` | The variable that holds the GitHub token. |
| `debug.directory` | | Required with the `directory` strategy. The directory that it writes the files to. |
| `defaults` | | The target of the instances that no mapping matches, and the keys that mappings leave out. |
| `mappings` | none | Each has a unique `name`, a `match` of attributes, and any keys of a target. |

The keys of a target, in `defaults` or in a mapping:

| Key | Default | |
|---|---|---|
| `repo` | | Required unless `skip`, `stdout`, or `directory`. The repository, as `owner/name`. |
| `branch` | `main` | The branch that holds the files. |
| `root` | `/` | The directory of the files. `/` is the top of the repository. |
| `strategy` | `push` | `push`, `pull_request`, `stdout`, or `directory`. |
| `sensitive` | `mask` | `mask` or `omit`. |
| `on_decommission` | `delete` | `delete` or `keep`. |
| `skip` | `false` | `true` writes nothing. |
| `commit.author` | `actor` | `actor` or `bot`. |
| `commit.email` | `noreply@massdriver.cloud` | The author's email with `actor`. |
| `commit.message` | `<instance>: <action> deployment <id>` | Text with `\(expr)`. |
| `pull_request.branch` | `massdriver/params/<project>-<environment>` | Text with `\(expr)`. |
| `pull_request.title` | `Massdriver params: <project>-<environment>` | Text with `\(expr)`. |
| `pull_request.rolling` | `true` | One open pull request for the branch, or one for each deployment. |
| `pull_request.reviewers` | none | Logins, and `org/team` for teams. |

A bad value, an unknown key, a mapping without `name` or `match`, or two mappings with one name stops the
automation at start.

The service account needs view access to the projects of the instances it syncs. It reads each instance's last
completed `PROVISION` deployment, its params, and its bundle's params schema.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name params-to-git \
  --url https://params-to-git.example.com/hooks/massdriver \
  --event-type deployment.completed \
  --condition '.data.metadata.deployment_action != "PLAN"'
```

The condition drops `PLAN` deployments, so Massdriver marks them `SKIPPED` and does not send them. With
`on_decommission: keep` everywhere, use `.data.metadata.deployment_action == "PROVISION"`. To sync one project only,
add `--attribute md-project=api`.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e GITHUB_TOKEN=github_pat_... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run params-to-git
```

## Limits

- Each event is handled before the response, within 8 seconds. A GitHub server error, a rate limit, a branch that
  moved during the write, or a Massdriver API error responds `500`, so Massdriver sends the event again. A missing
  repository or branch, or a token without access, is logged and not retried.
- Duplicate events are dropped by id for one hour, in memory. A repeat after that writes nothing, because the file
  is unchanged. Run one replica, so writes to a branch stay in order.
- The token is a personal access token or another bearer token. GitHub App credentials are not supported yet.
- A file larger than 1 MB cannot be read through the GitHub contents API.
