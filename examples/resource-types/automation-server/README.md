# automation-server (example)

An example resource type for the automations that one bundle instance runs. It is not an official Massdriver
resource type. Copy it to your own resource types repository, change it if you need to, and publish it:

```bash
mass resource-type create automation-server
mass resource-type publish massdriver.yaml
```

Both bundles produce an `automations` resource of this type when at least one automation is turned on. If you
rename the type or its fields, change the bundles' `resources` and `src/resources.tf` to match. It holds no
credentials.

## Fields

For each automation that is turned on:

| Field | |
|---|---|
| `name` | The automation, such as `statuspage`. |
| `url` | Where it is served, such as `https://drift.automations.acme.com`. |
| `destination` | The `id`, `name`, `event_types`, `attributes`, and `condition` of its event destination. |
| `endpoints` | URLs that people or other services use. |

| Automation | Endpoints |
|---|---|
| statuspage | Status page, Status JSON |
| cost-explorer | Dashboard, Costs JSON |
| proposal-chatops | Slack interactivity URL |
| drift | Start a check, when `DRIFT_RUN_TOKEN` is set |
| freeze-guard | Lift a window, when `FREEZE_GUARD_ADMIN_TOKEN` is set |
