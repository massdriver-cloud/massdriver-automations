# autodeploy-downstream

When an instance finishes a `PROVISION` deployment, deploy each instance that depends on it and has the
attribute `autodeploy=true`. A [rules file](#rules) selects instances by other attributes, and can propose the
deployments for review in place of running them.

Set the attribute on a component to opt that component in across every environment. Set it on an
environment or a project to opt in everything below it.

An instance depends on the upstream when one of its dependencies resolves to a resource that the upstream
produces. All three sources count:

- a link on the project blueprint
- an environment default, in any environment
- a remote reference, in any project

The automation skips an instance that is not `PROVISIONED`, so it never creates infrastructure for the first time.

A downstream deploy that completes sends its own `deployment.completed` event, so changes cascade through
the graph one level at a time.

## Configuration

| Variable | |
|---|---|
| `MASSDRIVER_SIGNING_SECRET` | Required. The `whsec_` signing secret of the event destination. |
| `MASSDRIVER_ORGANIZATION_ID` | Required. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | Required. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | The URL of your Massdriver installation. Default: `https://api.massdriver.cloud`. |
| `CONFIG_FILE` | The config file. Default: `/etc/massdriver-automations/config.yaml`. |

`PORT`, `LOG_FORMAT`, and `LOG_LEVEL` are in the [common settings](../../README.md#common-settings).

The config file is YAML. [`config.example.yaml`](config.example.yaml) has every key, with comments. An unknown key stops the automation at start.

| Key | Default | |
|---|---|---|
| `rules` | one rule, `autodeploy=true` | The [rules](#rules) that select downstream instances. |

Without a config file, or with no rules, one rule named `autodeploy=true` deploys the downstream instances whose attribute `autodeploy` is `true`.

The service account needs `instance:deploy` on the downstream instances of `deploy` rules, `instance:propose` on the downstream instances of `propose` rules, and view access to their projects.

## Rules

```yaml
rules:
  - name: staging-cascade
    upstream:
      md-environment: staging
    downstream:
      autodeploy: "true"
      md-environment: staging
    action: deploy
  - name: prod-review
    upstream:
      md-environment: prod
    downstream:
      autodeploy: "true"
      tier: [frontend, api]
    action: propose
```

| Field | | |
|---|---|---|
| `name` | Required. | Unique. The logs and the deployment message name the rule. |
| `downstream` | Required. | Attributes that the effective attributes of a downstream instance must have. |
| `upstream` | Optional. | Attributes that the upstream's `deployment.completed` event must have. |
| `action` | `deploy` | `deploy` runs a `PROVISION` deployment. `propose` creates a `PROVISION` deployment in `PROPOSED` status, which waits for a person to approve it. |

An instance must have every attribute of `downstream`, and the event must have every attribute of `upstream`. A list
of values matches any one of them. The first rule that matches an instance applies, and the others do not.

The deployment message names the rule:

```text
Auto-deploy (rule staging-cascade): upstream api-staging-db completed deployment 6f1c...
```

An unknown field, a rule without `name` or `downstream`, or two rules with one name stops the automation at start.

### Declare the attributes

System attributes, such as `md-environment` and `md-project`, need no declaration. Each other key in `downstream`
must be a custom attribute of your organization, with each value that the rules use, before you can set it. Declare
it at the `COMPONENT` scope to set it on components, with `required: false`, so that components without it stay
valid. `mass` has no command for custom attributes. Use the `createCustomAttribute` mutation of the V2 API, as an
account with `organization:manageCustomAttributes`:

```bash
curl https://api.massdriver.cloud/api/v2 \
  -H "Authorization: Bearer $MASSDRIVER_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"query": "mutation { createCustomAttribute(organizationId: \"acme\", input: {key: \"autodeploy\", scope: COMPONENT, required: false, values: [\"true\", \"false\"]}) { successful messages { field message } } }"}'
```

Custom attribute keys allow only letters, digits, and underscores.

## Event destination

Requires [mass](https://docs.massdriver.cloud/reference/cli/overview) 2.6.0 or later.

```bash
mass event destination create \
  --name autodeploy-downstream \
  --url https://autodeploy.example.com/hooks/massdriver \
  --event-type deployment.completed \
  --condition '.data.metadata.deployment_action == "PROVISION"'
```

The condition drops `PLAN` and `DECOMMISSION` deployments, so Massdriver marks them `SKIPPED` and does not send
them. To act on one environment only, add `--attribute md-environment=prod`.

Copy the signing secret from the destination's **Overview** tab into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  ghcr.io/massdriver-cloud/massdriver-automations:latest run autodeploy-downstream
```

## Limits

- Duplicate events are dropped by id for one hour, in memory. Run one replica.
- The automation responds before it deploys. A failed deploy is logged, and Massdriver does not retry it.
- Each downstream deploys with its saved params and its current version constraint.
