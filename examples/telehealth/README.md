# Telehealth example

Configs for Tendwell Health, a telehealth company with three teams and an app each: Visits (booking and
reminders), Care (charting), and Billing (claims). Each app has a `dev` and a `prod` environment.

| Config | What it does for Tendwell |
|---|---|
| [autodeploy-downstream](autodeploy-downstream.yaml) | Dev apps redeploy when a database, queue, or bucket they use changes. Prod gets a proposal for a person to approve. |
| [rollback-on-failure](rollback-on-failure.yaml) | A failed deploy of an app goes back to its last good deployment. |
| [drift](drift.yaml) | Prod is checked every night, and drift becomes a proposal that restores the deployed configuration. Dev is checked on request. |
| [freeze-guard](freeze-guard.yaml) | Prod changes wait for West Coast business hours. Platform Engineering stays exempt for overnight fixes. |
| [sandbox-on-join](sandbox-on-join.yaml) | Each person who joins a team gets a sandbox environment, deleted when they leave. |
| [statuspage](statuspage.yaml) | A status page for dev and one for prod, with the components patients and clinicians use. |
| [audit-archiver](audit-archiver.yaml) | Every event, as one JSON file, on disk. |
| [router](router.yaml) | Prints deploys, proposals, alarms, and team changes to the terminal, in color. |

The tests load every file here, so they stay valid.
