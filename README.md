# Massdriver automations

Small HTTP services that act on [Massdriver events](https://docs.massdriver.cloud/platform-operations/event-destinations).
Each directory is one service with its own `Dockerfile`. Copy the one you need.

| Automation | Events | Does |
|---|---|---|
| [autodeploy-downstream](autodeploy-downstream) | `deployment.completed` | Deploys opted-in dependents when an instance deploys. |

Every automation verifies the [Standard Webhooks](https://www.standardwebhooks.com) signature with the signing
secret of its event destination.
