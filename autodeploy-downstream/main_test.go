package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func sign(t *testing.T, req *http.Request, id string, body []byte) {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

func completedEvent(id, action, instance string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": "deployment.completed",
		"data": map[string]any{
			"entity":     map[string]any{"id": "dep-1"},
			"attributes": map[string]any{"md-instance": instance},
			"metadata":   map[string]any{"deployment_action": action},
		},
	})
	return b
}

func inst(id, status, autodeploy string, deps ...string) map[string]any {
	attrs := map[string]any{}
	if autodeploy != "" {
		attrs["autodeploy"] = autodeploy
	}
	ds := []map[string]any{}
	for _, d := range deps {
		ds = append(ds, map[string]any{"resource": map[string]any{"id": d}})
	}
	return map[string]any{"id": id, "status": status, "params": map[string]any{"size": "large"}, "effectiveAttributes": attrs, "dependencies": ds}
}

// fakeAPI serves an environment where api-prod-db produces resource "db-writer".
//
//	api-prod-app      link to db-writer, autodeploy=true      -> deployed
//	api-prod-worker   link to db-writer, no attribute         -> skipped
//	api-prod-cache    no dependency on db-writer               -> not downstream
//	api-prod-new      link to db-writer, autodeploy=true, INITIALIZED -> skipped
//	etl-prod-job      remote reference to db-writer, autodeploy=true -> deployed
//	ml-prod-train     env default of db-writer in ml-prod, autodeploy=true -> deployed
type fakeAPI struct {
	mu       sync.Mutex
	deployed map[string]map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)

	var data any
	switch {
	case strings.Contains(req.Query, "createDeployment"):
		f.mu.Lock()
		f.deployed[req.Variables["id"].(string)] = req.Variables["input"].(map[string]any)
		f.mu.Unlock()
		data = map[string]any{"createDeployment": map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": "new-dep"}}}
	case strings.Contains(req.Query, "remoteReferences"):
		data = map[string]any{"resource": map[string]any{
			"remoteReferences":    map[string]any{"cursor": map[string]any{"next": nil}, "items": []any{map[string]any{"instance": inst("etl-prod-job", "PROVISIONED", "true", "db-writer")}}},
			"environmentDefaults": map[string]any{"cursor": map[string]any{"next": nil}, "items": []any{map[string]any{"environment": map[string]any{"id": "ml-prod"}}}},
		}}
	case strings.Contains(req.Query, "environment(organizationId"):
		envs := map[string][]any{
			"api-prod": {
				inst("api-prod-db", "PROVISIONED", "true"),
				inst("api-prod-app", "PROVISIONED", "true", "db-writer"),
				inst("api-prod-worker", "PROVISIONED", "", "db-writer"),
				inst("api-prod-cache", "PROVISIONED", "true", "some-other-resource"),
				inst("api-prod-new", "INITIALIZED", "true", "db-writer"),
			},
			"ml-prod": {inst("ml-prod-train", "PROVISIONED", "true", "db-writer")},
		}
		data = map[string]any{"environment": map[string]any{"instances": envs[req.Variables["id"].(string)]}}
	default:
		data = map[string]any{"instance": map[string]any{
			"environment": map[string]any{"id": "api-prod"},
			"resources":   []any{map[string]any{"resource": map[string]any{"id": "db-writer"}}},
		}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func newReceiver(apiURL string) *receiver {
	return &receiver{
		cfg:  config{orgID: "acme", signingSecret: secret, attrKey: "autodeploy", attrValue: "true"},
		api:  &client{url: apiURL, apiKey: "md_test"},
		seen: map[string]time.Time{},
	}
}

func TestDeployDownstreamDeploysOptedInProvisionedConsumers(t *testing.T) {
	api := &fakeAPI{deployed: map[string]map[string]any{}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	r := newReceiver(srv.URL)
	var ev event
	_ = json.Unmarshal(completedEvent("evt-1", "PROVISION", "api-prod-db"), &ev)
	r.deployDownstream(ev, "api-prod-db")

	if len(api.deployed) != 3 {
		t.Fatalf("deployed %v, want api-prod-app, etl-prod-job, ml-prod-train", keys(api.deployed))
	}
	for _, id := range []string{"api-prod-app", "etl-prod-job", "ml-prod-train"} {
		input, ok := api.deployed[id]
		if !ok {
			t.Fatalf("%s was not deployed; deployed %v", id, keys(api.deployed))
		}
		if input["action"] != "PROVISION" || input["params"] != `{"size":"large"}` {
			t.Errorf("%s input = %v", id, input)
		}
		if input["message"] != "Auto-deploy: upstream api-prod-db completed deployment dep-1" {
			t.Errorf("%s message = %q", id, input["message"])
		}
	}
}

func TestHandle(t *testing.T) {
	r := newReceiver("http://127.0.0.1:0")

	cases := []struct {
		name   string
		body   []byte
		signed bool
		want   int
	}{
		{"unsigned", completedEvent("evt-a", "PROVISION", "api-prod-db"), false, http.StatusUnauthorized},
		{"plan is ignored", completedEvent("evt-b", "PLAN", "api-prod-db"), true, http.StatusNoContent},
		{"decommission is ignored", completedEvent("evt-c", "DECOMMISSION", "api-prod-db"), true, http.StatusNoContent},
		{"provision is accepted", completedEvent("evt-d", "PROVISION", "api-prod-db"), true, http.StatusAccepted},
		{"redelivery is dropped", completedEvent("evt-d", "PROVISION", "api-prod-db"), true, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(tc.body)))
			if tc.signed {
				var ev event
				_ = json.Unmarshal(tc.body, &ev)
				sign(t, req, ev.ID, tc.body)
			}
			rec := httptest.NewRecorder()
			r.handle(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func keys(m map[string]map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
