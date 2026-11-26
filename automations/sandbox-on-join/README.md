# sandbox-on-join

When a person joins a configured group, fork a template environment for them. When they leave, decommission
the fork and, if configured, delete it.

The fork gets two attributes:

- `owner=<email>`, the email of the person
- `sandbox=true`

The fork identifier comes from the local part of the email, lowercased, with every character other than `a-z`
and `0-9` removed, and cut to 20 characters. `Sam.Lee@example.com` in project `api` gets `api-samlee`. When the
project already has that environment, the next free one of `samlee2` to `samlee99` is used.

A person gets one sandbox for each template. When they join a group and already have a sandbox from the same
template, nothing happens.

When a person leaves a group, every sandbox they own from that group's templates is decommissioned, unless
another of their groups gives them the same template. A sandbox with no `PROVISIONED` or `FAILED` instances
is deleted right away if `deleteAfterDecommission` is set.

Each instance that finishes a `DECOMMISSION` sends a `deployment.completed` event. When the sandbox then has
no `PROVISIONED` or `FAILED` instances, its owner is not in a group that gives its template, and Massdriver
reports it as deletable, the sandbox is deleted. A sandbox that the owner decommissions while still in the
group is kept.

Before each action, the current group memberships of the person are read from the API. Events that arrive out
of order or twice do not fork or tear down a second time.

Service accounts that join a group are ignored.

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
| `sandboxes[].group` | | Required. The group name, as shown in Massdriver, or the group ID. |
| `sandboxes[].project` | | Required. The project of the template environment. Forks go into the same project. |
| `sandboxes[].environment` | | Required. The template environment. |
| `sandboxes[].deploy` | `false` | Deploy the fork after it is created. |
| `sandboxes[].deleteAfterDecommission` | `false` | Delete the fork after its decommission completes. |
| `sandboxes[].copyEnvironmentDefaults` | `false` | Copy the template's environment defaults. |
| `sandboxes[].copySecrets` | `false` | Copy the secrets of the template's instances. |
| `sandboxes[].copyRemoteReferences` | `false` | Copy the template's remote references. |

At least one rule is required. A group can have more than one rule.

The service account needs:

- `project:view` on the template's project and `environment:create` for environments with the attributes
  `owner` and `sandbox`
- `environment:deploy`, `environment:decommission`, and `environment:delete` on the forks
- with the `copy*` options: `resource:view` and a resource grant for each copied environment default and remote
  reference, and `instance:configure` on each template instance with secrets

If your organization declares custom attributes at the `ENVIRONMENT` scope, Massdriver accepts only declared
keys with declared values. Declare `owner` with the email of every person who can get a sandbox, and `sandbox`
with the value `true`. Without environment-scope custom attributes, any key and value is accepted.

## Event destination

```bash
mass event destination create \
  --name sandbox-on-join \
  --url https://sandbox.example.com/hooks/massdriver \
  --event-type group_membership.added \
  --event-type group_membership.removed \
  --event-type deployment.completed \
  --condition '((.type | startswith("group_membership.")) and .data.entity.account_id != null and .data.entity.group_name == "Developers") or (.type == "deployment.completed" and .data.metadata.deployment_action == "DECOMMISSION" and .data.attributes.sandbox == "true")'
```

List each configured group in the condition, for example
`(.data.entity.group_name | IN("Developers", "Contractors"))`, or `.data.entity.group_id` for a group named by ID. Do not set `--attribute`: group membership events
have no attributes, so an attribute filter drops them.

Copy the signing secret from the destination into `MASSDRIVER_SIGNING_SECRET`.

## Run

```bash
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/massdriver-automations/config.yaml:ro" \
  -e MASSDRIVER_ORGANIZATION_ID=acme \
  -e MASSDRIVER_API_KEY=md_... \
  -e MASSDRIVER_SIGNING_SECRET=whsec_... \
  ghcr.io/massdriver-cloud/massdriver-automations:0.2 run sandbox-on-join
```

## Limits

- Duplicate events are dropped by id for one hour, in memory. Run one replica.
- The automation responds before it forks, deploys, decommissions, or deletes. A failed call is logged, and
  Massdriver does not retry it.
- Deleting a group does not send `group_membership.removed` for its members. Their sandboxes stay.
- A person who joins a group before they register gets their sandbox when they accept the invitation.
- A decommission that fails leaves a `FAILED` instance, and the sandbox is not deleted. Decommission it again to
  finish the teardown.
- The owner is matched by email. When a person changes their email, the old sandbox is not found.
