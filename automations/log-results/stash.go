package logresults

import (
	"context"
	_ "embed"
	"strings"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

//go:embed queries/set_stash_entry.graphql
var setStashEntryMutation string

// setDeploymentEntry writes one entry to a deployment's stash with alphaSetStashEntry. It sends what
// massdriver.Client.SetStashEntry sends in massdriver-automations#30; once that merges, call it and delete this file.
func setDeploymentEntry(ctx context.Context, api *massdriver.Client, deploymentID string, e Entry) error {
	input := map[string]any{"value": e.Value, "format": strings.ToUpper(e.Format)}
	if e.Label != "" {
		input["label"] = e.Label
	}
	var out struct {
		Set massdriver.Payload[struct {
			Key string `json:"key"`
		}] `json:"alphaSetStashEntry"`
	}
	err := api.Do(ctx, setStashEntryMutation, map[string]any{
		"org":   api.OrganizationID,
		"scope": map[string]any{"deployment": deploymentID},
		"key":   e.Key,
		"input": input,
	}, &out)
	if err != nil {
		return err
	}
	return out.Set.Err()
}
