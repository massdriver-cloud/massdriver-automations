package automations_test

import (
	"path/filepath"
	"testing"

	auditarchiver "github.com/massdriver-cloud/massdriver-automations/automations/audit-archiver"
	autodeploydownstream "github.com/massdriver-cloud/massdriver-automations/automations/autodeploy-downstream"
	"github.com/massdriver-cloud/massdriver-automations/automations/drift"
	freezeguard "github.com/massdriver-cloud/massdriver-automations/automations/freeze-guard"
	rollbackonfailure "github.com/massdriver-cloud/massdriver-automations/automations/rollback-on-failure"
	"github.com/massdriver-cloud/massdriver-automations/automations/router"
	sandboxonjoin "github.com/massdriver-cloud/massdriver-automations/automations/sandbox-on-join"
	stashsync "github.com/massdriver-cloud/massdriver-automations/automations/stash-sync"
	"github.com/massdriver-cloud/massdriver-automations/automations/statuspage"
	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

func TestTelehealthExamplesLoad(t *testing.T) {
	dir := filepath.Join("..", "examples", "telehealth")
	rollback := rollbackonfailure.DefaultConfig()
	configs := map[string]any{
		"audit-archiver":        &auditarchiver.Config{},
		"autodeploy-downstream": &autodeploydownstream.Config{},
		"drift":                 &drift.Config{},
		"freeze-guard":          &freezeguard.Config{},
		"rollback-on-failure":   &rollback,
		"router":                &router.Config{},
		"sandbox-on-join":       &sandboxonjoin.Config{},
		"stash-sync":            &stashsync.Config{},
		"stash-sync-incident":   &stashsync.Config{},
		"stash-sync-rollout":    &stashsync.Config{},
		"stash-sync-steady":     &stashsync.Config{},
	}
	for name, cfg := range configs {
		if err := automation.LoadConfigFile(filepath.Join(dir, name+".yaml"), cfg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := statuspage.LoadConfig(filepath.Join(dir, "statuspage.yaml")); err != nil {
		t.Errorf("statuspage: %v", err)
	}
}
