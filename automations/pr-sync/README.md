# pr-sync

On an interval, reads the pull requests of the GitHub repositories that belong to a Massdriver project, and writes
them to one JSON entry in the project's stash. A [note](https://docs.massdriver.cloud/guides/annotations/notes)
pinned to the project can then list what is waiting for review, what is running checks, and what is merging.

## The entry

The key is `github/prs` in each project's stash. The value:

```json
{
  "prs": [
    {
      "repo": "visits",
      "number": 412,
      "title": "Add appointment reminders",
      "author": "lee",
      "url": "https://github.com/tendwell-health/visits/pull/412",
      "state": "checks",
      "draft": false,
      "review": "approved",
      "checks": "pending",
      "updated_at": "2026-11-27T08:58:10Z",
      "merged_at": null
    }
  ],
  "updated_at": "2026-11-27T09:00:00Z"
}
```

| Field | |
|---|---|
| `repo` | The repository name, without the owner. |
| `state` | One word for where the pull request is. See the next table. |
| `draft` | `true` for a draft. |
| `review` | GitHub's review decision: `approved`, `changes_requested`, `review_required`, or `none` when the branch needs no review. |
| `checks` | The combined state of the checks and statuses on the last commit: `success`, `failure`, `error`, `pending`, `expected`, or `none`. |
| `updated_at`, `merged_at` | UTC times. `merged_at` is `null` for an open pull request. |
| `updated_at` (top level) | When the automation wrote the entry. |

`state` is the first that applies:

| State | When |
|---|---|
| `merged` | It merged within `merged_window`. |
| `draft` | It is a draft. |
| `merging` | It is in the merge queue. |
| `failed` | A reviewer requested changes, or a check failed. |
| `merging` | Auto-merge is on. |
| `checks` | Checks are running. |
| `approved` | It is approved, and its checks passed or it has none. |
| `review` | Anything else: it waits for a review. |

The list has open pull requests first, in the order `merging`, `checks`, `failed`, `approved`, `review`, `draft`,
each by last update, newest first. Merged pull requests come next, newest merge first. The list has at most
`max_prs` pull requests.

## A note on the project

Pin this note to the project:

```liquid
### Merging PRs
{% for pr in project.stash["github/prs"].prs %}
- {% case pr.state %}{% when "merged" %}✅{% when "merging" %}🔄{% when "checks" %}🧪{% when "failed" %}❌{% else %}⏳{% endcase %} [{{ pr.repo }}#{{ pr.number }}]({{ pr.url }}) {{ pr.title }} · _{{ pr.author }}_ · **{{ pr.state }}**
{% endfor %}
```

## How it writes

- Each cycle, the automation reads each repository once with one GitHub GraphQL query, even when several projects
  list it. The query returns the 50 most recently updated open pull requests and the 50 most recently updated
  merged ones.
- It writes a project's entry when the list changed. An entry that does not change is written again in the
  second cycle after its last write.
- Every write sets a TTL of three intervals. When the automation stops, its entries expire.
- When a repository's query fails, the automation does not write the entries of the projects that list it, so
  their pull requests do not seem to go away. The last entry stays until its TTL. The other projects are still
  written.

The automation keeps what it wrote in memory, and the bundles run one replica. A second replica is safe: both write
the same value.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |
| `GITHUB_TOKEN` | Required, or the variable that `github.token_env` names. A GitHub token that can read the repositories' pull requests, commit statuses, and checks. Fine-grained: `Pull requests`, `Commit statuses`, and `Contents` read. Classic: the `repo` scope for private repositories. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

```yaml
interval: 2m
key: github/prs
label: Pull requests
merged_window: 24h
max_prs: 20
projects:
  - project: visits
    repos: [tendwell-health/visits, tendwell-health/infra]
```

| Key | Default | |
|---|---|---|
| `interval` | `2m` | How often to read GitHub. A Go duration, at least `30s`. |
| `key` | `github/prs` | The stash key in each project. Letters, digits, and `_ . / -`, up to 255 characters. |
| `label` | `Pull requests` | The entry's name in the Massdriver UI. |
| `merged_window` | `24h` | How long a merged pull request stays in the list. `0s` lists only open pull requests. |
| `max_prs` | `20` | The most pull requests in one entry, from 1 to 50. |
| `github.graphql_url` | `https://api.github.com/graphql` | For GitHub Enterprise Server, `https://<host>/api/graphql`. |
| `github.token_env` | `GITHUB_TOKEN` | The variable that holds the GitHub token. |
| `projects[].project` | | Required. The project identifier, such as `visits`. |
| `projects[].repos` | | Required. The repositories, as `owner/name`. |

## Permissions

The service account needs `stash:write` on each project in `projects`.

## Event destination

The automation reads GitHub on an interval, and acts on no event. Every automation receives events from a
destination, so create one that sends nothing:

```bash
mass event destination create \
  --name pr-sync \
  --url https://pr-sync.example.com/hooks/massdriver \
  --event-type deployment.completed \
  --condition 'false'
```

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e GITHUB_TOKEN=github_pat_... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run pr-sync
```

## Limits

- The stash API is alpha. Its names start with `alpha` and can change.
- A repository with more than 50 open pull requests lists only the 50 most recently updated. An older one in the
  merge queue is left out.
- Each repository costs about one point of GitHub's GraphQL rate limit each cycle, out of 5,000 an hour. Ten
  repositories every two minutes use about 300 an hour.
- Authentication is by token. GitHub App authentication is not supported.
