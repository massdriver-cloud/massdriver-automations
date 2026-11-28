package massdriver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestDeleteStashEntry(t *testing.T) {
	var got map[string]any
	reply := `{"data":{"alphaDeleteStashEntry":{"successful":true,"messages":[]}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = req.Variables
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}

	if err := c.DeleteStashEntry(t.Context(), "environment", "api-prod", "md/v1/scorecard/gold"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"org": "acme", "scope": map[string]any{"environment": "api-prod"}, "key": "md/v1/scorecard/gold"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("variables = %v", got)
	}

	reply = `{"data":{"alphaDeleteStashEntry":{"successful":false,"messages":[{"field":"scope","message":"forbidden"}]}}}`
	if err := c.DeleteStashEntry(t.Context(), "environment", "api-prod", "x"); err == nil {
		t.Error("a refused delete: want an error")
	}
}
