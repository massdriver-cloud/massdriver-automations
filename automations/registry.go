// Package automations lists every automation that the massdriver-automations binary can run.
package automations

import (
	"github.com/massdriver-cloud/massdriver-automations/automations/attribute-enforcer"
	"github.com/massdriver-cloud/massdriver-automations/automations/audit-archiver"
	"github.com/massdriver-cloud/massdriver-automations/automations/autodeploy-downstream"
	"github.com/massdriver-cloud/massdriver-automations/automations/bundle-scanner"
	"github.com/massdriver-cloud/massdriver-automations/automations/freeze-guard"
	"github.com/massdriver-cloud/massdriver-automations/automations/github-deployments"
	"github.com/massdriver-cloud/massdriver-automations/automations/proposal-chatops"
	"github.com/massdriver-cloud/massdriver-automations/automations/release-notes"
	"github.com/massdriver-cloud/massdriver-automations/automations/router"
	"github.com/massdriver-cloud/massdriver-automations/automations/sandbox-on-join"
	"github.com/massdriver-cloud/massdriver-automations/automations/statuspage"
	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// All is every automation, by name.
var All = []automation.Definition{
	attributeenforcer.Definition,
	auditarchiver.Definition,
	autodeploydownstream.Definition,
	bundlescanner.Definition,
	freezeguard.Definition,
	githubdeployments.Definition,
	proposalchatops.Definition,
	releasenotes.Definition,
	router.Definition,
	sandboxonjoin.Definition,
	statuspage.Definition,
}
