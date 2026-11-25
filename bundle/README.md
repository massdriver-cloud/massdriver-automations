# massdriver-automations bundle

Runs the [automations](../README.md) on a Kubernetes cluster. Turn on any set of them. For each enabled automation,
the bundle creates:

- a Deployment with one replica, running `massdrivercloud/massdriver-automations:<tag> run <name>`
- a Secret with its environment variables, and a ConfigMap with its YAML configuration file, if it has one
- a Service and an Ingress for the host `<name>.<domain>`
- a Massdriver event destination that sends events to `https://<name>.<domain>/hooks/massdriver`, with the event
  types, attributes, and condition from the automation's README
- for statuspage, a PersistentVolumeClaim for `STATE_DIR`

The signing secret of each event destination goes into the automation's Secret as `MASSDRIVER_SIGNING_SECRET`.
The bundle produces no resources for other bundles.

## Requirements

- The `massdriver-cloud/massdriver` provider 2.7.0 or later. 2.7.0 adds `signing_secret` to
  `massdriver_event_destination`
  ([terraform-provider-massdriver#56](https://github.com/massdriver-cloud/terraform-provider-massdriver/pull/56)).
  The bundle does not deploy until 2.7.0 is published.
- A `kubernetes-cluster` dependency. The bundle deploys into the namespace of that resource, or `default` when it
  has none. It does not create the namespace.
- An ingress controller, and a wildcard DNS record for `*.<domain>` that points at it.
- TLS on every host. Massdriver and Slack send requests to `https://` URLs. Set a cert-manager ClusterIssuer, set a
  TLS secret that holds a certificate for `*.<domain>`, or end TLS in front of the ingress controller.
- A service account token in the `MASSDRIVER_API_KEY` secret.

## Ingress host

Set **Ingress > Domain**, such as `automations.acme.com`. Each automation is served at `<name>.<domain>`:

| Automation | Host | Paths that must be public |
|---|---|---|
| audit-archiver | `audit-archiver.<domain>` | `/hooks/massdriver` |
| autodeploy-downstream | `autodeploy-downstream.<domain>` | `/hooks/massdriver` |
| bundle-scanner | `bundle-scanner.<domain>` | `/hooks/massdriver` |
| decommission-cleanup | `decommission-cleanup.<domain>` | `/hooks/massdriver` |
| freeze-guard | `freeze-guard.<domain>` | `/hooks/massdriver` |
| github-deployments | `github-deployments.<domain>` | `/hooks/massdriver` |
| proposal-chatops | `proposal-chatops.<domain>` | `/hooks/massdriver`, `/slack/interactions` |
| release-notes | `release-notes.<domain>` | `/hooks/massdriver` |
| rollback-on-failure | `rollback-on-failure.<domain>` | `/hooks/massdriver` |
| sandbox-on-join | `sandbox-on-join.<domain>` | `/hooks/massdriver` |
| statuspage | **statuspage > Host**, or `statuspage.<domain>` | every path |

Each Ingress sends every path of its host to the automation. Two instances of this bundle need different domains,
because each host serves one automation.

For proposal-chatops, set the Slack app's interactivity request URL to
`https://proposal-chatops.<domain>/slack/interactions`.

## Service account

The bundle creates event destinations with the service account token, because a deployment cannot create them. The
automations that call the Massdriver API get the same token as `MASSDRIVER_API_KEY`. Set
**Massdriver > Organization ID** to the organization of the service account.

The service account needs `organization:manageEventDestinations`, plus the permissions of each automation that you
turn on:

| Automation | Permissions |
|---|---|
| audit-archiver | None. |
| autodeploy-downstream | `instance:deploy` on the downstream instances, and `project:view` on their projects. |
| bundle-scanner | `repo:view` and `repo:pull` on the bundle repos. With a result attribute key, also `repo:update`. |
| decommission-cleanup | None. |
| freeze-guard | `instance:deploy` and `instance:decommission` on the instances that the windows cover. |
| github-deployments | None. |
| proposal-chatops | `instance:deploy` on the instances. |
| release-notes | `repo:view` on the bundle repos, and `project:view` on the projects whose instances are listed. |
| rollback-on-failure | `instance:propose` and `instance:deploy` on the opted-in instances, and `project:view` on their projects. |
| sandbox-on-join | `project:view` on the template's project, `environment:create` for environments with the attributes `owner` and `sandbox`, and `environment:deploy`, `environment:decommission`, and `environment:delete` on the forks. With the `copy*` options, also `resource:view`, a resource grant for each copied environment default and remote reference, and `instance:configure` on each template instance with secrets. |
| statuspage | `project:view` on the projects of the configured environments. |

## Secrets

Each secret is set on the instance. Set only the secrets of the automations that you turn on.

| Secret | Automation | Variable |
|---|---|---|
| `MASSDRIVER_API_KEY` | all that call the API | `MASSDRIVER_API_KEY` |
| `AUDIT_ARCHIVER_AWS_ACCESS_KEY_ID`, `AUDIT_ARCHIVER_AWS_SECRET_ACCESS_KEY` | audit-archiver | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` |
| `AUDIT_ARCHIVER_SPLUNK_HEC_TOKEN` | audit-archiver | `SPLUNK_HEC_TOKEN` |
| `AUDIT_ARCHIVER_DD_API_KEY` | audit-archiver | `DD_API_KEY` |
| `BUNDLE_SCANNER_SLACK_WEBHOOK_URL` | bundle-scanner | `SLACK_WEBHOOK_URL` |
| `DECOMMISSION_CLEANUP_HOOK_ENV` | decommission-cleanup | Each key of the JSON object |
| `FREEZE_GUARD_SLACK_WEBHOOK_URL` | freeze-guard | `SLACK_WEBHOOK_URL` |
| `GITHUB_DEPLOYMENTS_TOKEN` | github-deployments | `GITHUB_TOKEN` |
| `PROPOSAL_CHATOPS_SLACK_BOT_TOKEN`, `PROPOSAL_CHATOPS_SLACK_SIGNING_SECRET` | proposal-chatops | `SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET` |
| `RELEASE_NOTES_SLACK_WEBHOOK_URL`, `RELEASE_NOTES_TEAMS_WEBHOOK_URL` | release-notes | `SLACK_WEBHOOK_URL`, `TEAMS_WEBHOOK_URL` |
| `ROLLBACK_ON_FAILURE_SLACK_WEBHOOK_URL` | rollback-on-failure | `SLACK_WEBHOOK_URL` |

## Event destinations

Each destination is named `<instance>-<automation>`. The bundle writes the condition from the configuration where
the automation's README says to:

- sandbox-on-join: from the `group` of each sandbox rule.
- statuspage: from the environment `id`s of the configuration.
- github-deployments: from the repository attribute key.
- rollback-on-failure: from the opt-in attribute key and value.

**Event attributes** on an automation adds an attribute filter to its destination, such as
`md-environment` = `prod`. audit-archiver, github-deployments, and sandbox-on-join have none, because their events
can have no attributes. release-notes always filters on `md-repo-artifact-type` = `bundle`.

## Limits

- Each automation runs one replica and restarts on every change of its settings. The automations keep their
  queues and repeated-event ids in memory, and a restart loses them.
- decommission-cleanup runs `http` hooks. The image has no shell or tools, so `exec` hooks need an image that you
  build, set in **Image > Repository**. That image is used for every automation.
- audit-archiver reads S3 credentials from the secrets. It does not set up IRSA or EKS Pod Identity.
- Resource requests and limits are fixed: 25m CPU and 64Mi memory, up to 500m and 256Mi, and for bundle-scanner
  250m and 512Mi, up to 2 CPUs and 2Gi.
