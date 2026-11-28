# leaderboard

On an interval, reads the pull requests of GitHub repositories, ranks their authors, and writes the top players to
the leaderboard widget of Massdriver environments. Routes in the config choose which repositories, branches,
labels, and paths count for which environments, over what window, and how a pull request scores. A route can
also write a fixed list, so you can try the widget without GitHub.

## The entry

The key is `md/v1/leaderboard` in each environment's stash. The value is the widget's people list, best first:

```json
{
  "title": "Most merged this month",
  "entries": [
    {
      "name": "Jo Park",
      "image_url": "https://avatars.githubusercontent.com/u/9919?v=4",
      "handles": ["github:jopark"]
    },
    {
      "name": "lee",
      "image_url": "https://avatars.githubusercontent.com/u/1234?v=4",
      "handles": ["github:lee"]
    }
  ]
}
```

| Field | |
|---|---|
| `title` | The route's `title`. Left out when the route has none. |
| `entries[].name` | The author's GitHub name, or the login when the profile has no name. |
| `entries[].image_url` | The author's GitHub avatar. |
| `entries[].handles` | `github:<login>`. |

A route with `static` writes its list as it is.

## Scores

| `score` | A player gets |
|---|---|
| `merged` | One point for each pull request merged within `window`. The default. |
| `opened` | One point for each pull request opened within `window`, in any state. |
| `lines` | The lines added and deleted by the pull requests merged within `window`. |

Scores add up across the route's repositories. Players with the same score are in order of login. A player with no
points is not on the board.

Filters narrow which pull requests score:

- `base` keeps the pull requests into one branch.
- `labels` keeps the pull requests with any of the labels.
- `paths` keeps the pull requests that change a file under any of the prefixes, such as `services/billing/`.
- `exclude_authors` removes logins, such as a bot account that is a user.
- GitHub Apps, such as dependabot and renovate, do not score unless `include_bots` is `true`.

## How it writes

- Each cycle, the automation runs each distinct GitHub query once, even when several routes need it. A query reads
  a repository's pull requests, most recently updated first, 50 to a page, until a page reaches pull requests last
  updated before the window.
- It writes an environment's entry when the list changed. An entry that does not change is written again in the
  second cycle after its last write.
- Every write sets a TTL of three intervals. When the automation stops, its entries expire.
- When a repository's query fails, the automation does not write the leaderboards of the routes that list it, so
  players do not seem to drop. The last entry stays until its TTL.
- A route with no players writes nothing. The widget needs at least one person.

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
| `GITHUB_TOKEN` | Required when a route has `pull_requests`, or the variable that `github.token_env` names. A GitHub token that can read the repositories' pull requests. Fine-grained: `Pull requests` and `Contents` read. Classic: the `repo` scope for private repositories. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key
stops the automation at start.

```yaml
interval: 15m
routes:
  - environments: [billing-prod, billing-staging]
    title: Most merged this month
    pull_requests:
      repos: [tendwell-health/billing, tendwell-health/infra]
      base: main
      window: 720h
  - environments: [sandbox-dev]
    title: Demo leaderboard
    static:
      - name: Jo Park
        handles: [github:jopark, slack:@jo]
      - name: Lee Chen
        email: lee@tendwell.example
```

| Key | Default | |
|---|---|---|
| `interval` | `15m` | How often to read GitHub and write. A Go duration, at least `1m`. |
| `label` | `Leaderboard` | The entry's name in the Massdriver UI. |
| `github.graphql_url` | `https://api.github.com/graphql` | For GitHub Enterprise Server, `https://<host>/api/graphql`. |
| `github.token_env` | `GITHUB_TOKEN` | The variable that holds the GitHub token. |
| `routes[].environments` | | Required. Environment identifiers, such as `billing-prod`. An environment can be in one route only. |
| `routes[].title` | | The card's title, up to 64 characters. |
| `routes[].max_players` | `10` | The most players, from 1 to 25. |
| `routes[].pull_requests.repos` | | Required. The repositories, as `owner/name`. |
| `routes[].pull_requests.base` | | Only pull requests into this branch. |
| `routes[].pull_requests.labels` | | Only pull requests with any of these labels. |
| `routes[].pull_requests.paths` | | Only pull requests that change a file under any of these prefixes. |
| `routes[].pull_requests.window` | `720h` | How far back to count. A Go duration, at least `1h`. |
| `routes[].pull_requests.score` | `merged` | `merged`, `opened`, or `lines`. See [Scores](#scores). |
| `routes[].pull_requests.exclude_authors` | | GitHub logins that never score. |
| `routes[].pull_requests.include_bots` | `false` | Whether GitHub Apps score. |
| `routes[].static` | | 1 to 25 people, written as they are. Each has at least one of `name` (up to 64 characters), `image_url` (http or https), `email` (one plain address), and `handles` (1 to 10, such as `slack:@jo` or `github:jo`). |

A route sets exactly one of `pull_requests` or `static`.

## Permissions

The service account needs `stash:write` on each environment in `routes`.

## Event destination

The automation reads GitHub on an interval, and acts on no event. Every automation receives events from a
destination, so create one that sends nothing:

```bash
mass event destination create \
  --name leaderboard \
  --url https://leaderboard.example.com/hooks/massdriver \
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
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run leaderboard
```

## Limits

- The stash API is alpha. Its names start with `alpha` and can change.
- One query reads at most 1,000 pull requests of a repository. A busy repository with a long window can score
  fewer pull requests than it merged. The automation then logs `github query truncated` at `warn`.
- `paths` looks at the first 100 files of each pull request.
- Each page costs about one point of GitHub's GraphQL rate limit, out of 5,000 an hour. Ten repositories with 100
  merged pull requests a month, read every 15 minutes, use about 120 an hour.
- Authentication is by token. GitHub App authentication is not supported.
