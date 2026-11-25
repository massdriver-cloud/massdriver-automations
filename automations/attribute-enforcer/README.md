# attribute-enforcer

When someone creates a project, an environment, or a component, check its attributes against a list of required
keys. For each key that is missing, set a default value, send a Slack message, or both. The Slack message names
the person or service account that created the entity.

The check uses the attributes that the entity had when Massdriver recorded the event, including the attributes
it inherits. A key set on a project counts for the environments and components of that project.

| Event | Entity |
|---|---|
| `project.created` | A new project. |
| `project.cloned` | A project made from another project's blueprint. |
| `environment.created` | A new environment, including a fork of an environment. |
| `component.created` | A new component on a project blueprint. |

For a missing key with a `default`, the automation reads the entity again. If the key is still missing, the automation
sends the entity's own attributes plus the defaults with `updateProject`, `updateEnvironment`, or
`updateComponent`. A key with a value that is not in `allowed` is only reported. The automation never changes a
value that someone set.

## Custom attributes

Massdriver accepts only attribute keys that your organization declares as custom attributes, at the scope of the
entity, with the value in the custom attribute's `values`. Declare each key before you add it to the
configuration. Use the same case as the declaration.

```graphql
mutation {
  createCustomAttribute(
    organizationId: "acme"
    input: { key: "TEAM", scope: PROJECT, required: false, values: ["platform", "payments"] }
  ) {
    successful
    messages { field message }
  }
}
```

A custom attribute with `required: true` makes Massdriver refuse to create an entity without the key. Use this
automation for keys that are declared with `required: false`, to fill them after the fact or to tell people about
them. A key that a project-scope custom attribute declares can only be set on projects: put it under `project`,
not under `environment` or `component`.

## Configuration

| Variable | Default | |
|---|---|---|
| `MASSDRIVER_SIGNING_SECRET` | | Required. The `whsec_` signing secret of the event destination. |
| `ATTRIBUTE_ENFORCER_CONFIG` | `/etc/attribute-enforcer/config.yaml` | The path of the configuration file. |
| `MASSDRIVER_ORGANIZATION_ID` | | Required with `set_default`. Your organization's identifier. |
| `MASSDRIVER_API_KEY` | | Required with `set_default`. An `md_` access token of a service account. |
| `MASSDRIVER_URL` | `https://api.massdriver.cloud` | The URL of your Massdriver installation. |
| `SLACK_WEBHOOK_URL` | | Required with `notify`. A Slack incoming webhook URL. |
| `PORT` | `8080` | |

With `set_default`, the service account needs `project:view`, `project:update` for projects,
`environment:update` for environments, and `project:design` for components. Its policies must allow the default
values.

### Configuration file

```yaml
# set_default, notify, or both.
actions: [set_default, notify]

project:
  - key: TEAM
    default: platform
  - key: COST_CENTER
    allowed: [cc-100, cc-200]

environment:
  - key: TIER
    default: standard
    allowed: [standard, critical]

component:
  - key: OWNER
```

| Field | |
|---|---|
| `key` | Required. The attribute key. |
| `default` | The value to set when the key is missing. Must be in `allowed` when both are set. |
| `allowed` | The values that comply. When empty, any value complies. |

For the project above, created by Jane Doe with `COST_CENTER=cc-999` and no `TEAM`, the Slack message is:

```
Project `api` was created by Jane Doe without the required attributes.
• `COST_CENTER` is `cc-999`. Allowed: `cc-100`, `cc-200`.
• `TEAM` was missing. Set it to `platform`.
```

## Event destination

The condition sends only the entities that break a rule, so Massdriver marks the compliant ones `SKIPPED`. Write
it from your configuration: a key without `allowed` is `== null`, and a key with `allowed` is
`| IN(...) | not`. This one matches the configuration file above.

```bash
mass event destination create \
  --name attribute-enforcer \
  --url https://attribute-enforcer.example.com/hooks/massdriver \
  --event-type project.created \
  --event-type project.cloned \
  --event-type environment.created \
  --event-type component.created \
  --condition '.data.attributes as $a
    | if (.type | startswith("project.")) then ($a.TEAM == null or ($a.COST_CENTER | IN("cc-100", "cc-200") | not))
      elif .type == "environment.created" then ($a.TIER | IN("standard", "critical") | not)
      else $a.OWNER == null end'
```

To check one project only, add `--attribute md-project=api`.

Copy the signing secret from the **Overview** tab of the destination into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e SLACK_WEBHOOK_URL=https://hooks.slack.com/services/... \
  -v "$PWD/config.yaml:/etc/attribute-enforcer/config.yaml:ro" \
  massdrivercloud/massdriver-automations:latest run attribute-enforcer
```

## Limits

- Duplicate events are dropped by id for one hour, in memory. Run one replica.
- The automation responds before it sets defaults and sends the message. A failed update is in the Slack message
  and the log, and Massdriver does not retry it.
- `project.cloned` is one event for the whole clone. The components that the clone copies send no
  `component.created` event, so the automation does not check them.
- The Slack message names the actor by name. The event has no email or Slack user, so the message cannot
  mention the person.
- Setting a default records a `project.updated`, `environment.updated`, or `component.updated` event.
