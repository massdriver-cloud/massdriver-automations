// Package automations lists every automation that the massdriver-automations binary can run.
package automations

import (
	auditarchiver "github.com/massdriver-cloud/massdriver-automations/automations/audit-archiver"
	autodeploydownstream "github.com/massdriver-cloud/massdriver-automations/automations/autodeploy-downstream"
	bundlescanner "github.com/massdriver-cloud/massdriver-automations/automations/bundle-scanner"
	chaosmonkey "github.com/massdriver-cloud/massdriver-automations/automations/chaos-monkey"
	costexplorer "github.com/massdriver-cloud/massdriver-automations/automations/cost-explorer"
	"github.com/massdriver-cloud/massdriver-automations/automations/drift"
	freezeguard "github.com/massdriver-cloud/massdriver-automations/automations/freeze-guard"
	githubdeployments "github.com/massdriver-cloud/massdriver-automations/automations/github-deployments"
	proposalchatops "github.com/massdriver-cloud/massdriver-automations/automations/proposal-chatops"
	releasenotes "github.com/massdriver-cloud/massdriver-automations/automations/release-notes"
	rollbackonfailure "github.com/massdriver-cloud/massdriver-automations/automations/rollback-on-failure"
	"github.com/massdriver-cloud/massdriver-automations/automations/router"
	sandboxonjoin "github.com/massdriver-cloud/massdriver-automations/automations/sandbox-on-join"
	staleproposals "github.com/massdriver-cloud/massdriver-automations/automations/stale-proposals"
	"github.com/massdriver-cloud/massdriver-automations/automations/statuspage"
	upgradetrain "github.com/massdriver-cloud/massdriver-automations/automations/upgrade-train"
	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// All is every automation, by name.
var All = []automation.Definition{
	auditarchiver.Definition,
	autodeploydownstream.Definition,
	bundlescanner.Definition,
	chaosmonkey.Definition,
	costexplorer.Definition,
	drift.Definition,
	freezeguard.Definition,
	githubdeployments.Definition,
	proposalchatops.Definition,
	releasenotes.Definition,
	rollbackonfailure.Definition,
	router.Definition,
	sandboxonjoin.Definition,
	staleproposals.Definition,
	statuspage.Definition,
	upgradetrain.Definition,
}
