package bundlescanner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

const mainTF = `resource "aws_s3_bucket" "b" { acl = "public-read" }`

const trivyOutput = `{"Results":[
 {"Target":".","MisconfSummary":{"Successes":3,"Failures":0}},
 {"Target":"src/main.tf","Misconfigurations":[
  {"ID":"AWS-0086","Title":"S3 Access block should block public ACL","Severity":"HIGH","Status":"FAIL","CauseMetadata":{"StartLine":1}},
  {"ID":"AWS-0089","Title":"Bucket has logging disabled","Severity":"LOW","Status":"FAIL","CauseMetadata":{"StartLine":1}},
  {"ID":"AWS-0090","Title":"Bucket versioning","Severity":"MEDIUM","Status":"PASS","CauseMetadata":{"StartLine":1}}]}]}`

const checkovOutput = `[
 {"check_type":"terraform","results":{"failed_checks":[
  {"check_id":"CKV_AWS_20","check_name":"S3 Bucket has an ACL defined which allows public READ access.","file_path":"/src/main.tf","severity":null,"file_line_range":[1,1]}]}},
 {"check_type":"kubernetes","results":{"failed_checks":[]}}]`

func repoPublished(id, repo, artifactType string, at time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": "repo.published",
		"time": at.Format(time.RFC3339),
		"data": map[string]any{
			"entity":     map[string]any{"name": repo, "artifact_type": artifactType},
			"attributes": map[string]any{"md-repo": repo},
			"metadata":   map[string]any{},
		},
	})
	return b
}

func post(t *testing.T, h http.Handler, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	var ev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &ev)
	automation.SignRequest(req, secret, ev.ID, body, time.Now())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// fakeMassdriver serves the V2 API and the OCI registry. Repo aws-aurora has 1.2.3, published before the event,
// and 1.2.4, published after it.
type fakeMassdriver struct {
	layers  map[string]string
	mu      sync.Mutex
	updates []map[string]any
	auth    []string
}

func (f *fakeMassdriver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Method+" "+r.URL.Path+" "+r.Header.Get("authorization"))
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/api/v2":
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		var data any
		switch {
		case strings.Contains(req.Query, "query RecentVersions"):
			data = map[string]any{"ociRepo": map[string]any{
				"artifactType": bundleArtifactType,
				"attributes":   map[string]any{"owner": "platform"},
				"tags": map[string]any{"items": []any{
					map[string]any{"tag": "1.2.4", "createdAt": "2026-11-24T06:00:00Z"},
					map[string]any{"tag": "1.2.3", "createdAt": "2026-11-24T05:54:02Z"},
				}},
			}}
		case strings.Contains(req.Query, "mutation UpdateRepoAttributes"):
			f.mu.Lock()
			f.updates = append(f.updates, req.Variables)
			f.mu.Unlock()
			data = map[string]any{"updateOciRepo": map[string]any{"successful": true, "messages": []any{}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})

	case r.URL.Path == "/v2/acme/aws-aurora/manifests/1.2.3":
		if r.Header.Get("accept") != ociManifestType {
			http.Error(w, "accept", http.StatusNotAcceptable)
			return
		}
		var layers []any
		for name, content := range f.layers {
			layers = append(layers, map[string]any{
				"digest":      digest(content),
				"size":        len(content),
				"annotations": map[string]any{titleAnnotation: name},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "layers": layers})

	case strings.HasPrefix(r.URL.Path, "/v2/acme/aws-aurora/blobs/"):
		d := strings.TrimPrefix(r.URL.Path, "/v2/acme/aws-aurora/blobs/")
		for _, content := range f.layers {
			if digest(content) == d {
				_, _ = io.WriteString(w, content)
				return
			}
		}
		http.NotFound(w, r)

	default:
		http.NotFound(w, r)
	}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type fakeSlack struct {
	mu    sync.Mutex
	texts []string
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var msg struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&msg)
	f.mu.Lock()
	f.texts = append(f.texts, msg.Text)
	f.mu.Unlock()
}

// fakeTool writes a script that copies the scanned src/main.tf to seen and prints output, as the scanner would.
func fakeTool(t *testing.T, name, output string, exitCode int, parse func([]byte) ([]finding, error)) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	out := filepath.Join(dir, "out.json")
	if err := os.WriteFile(out, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat src/main.tf > '" + seen + "'\ncat '" + out + "'\nexit " + strconv.Itoa(exitCode) + "\n"
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Tool{Name: name, Bin: bin, Args: tools[name].Args, Parse: parse}, seen
}

type harness struct {
	scanner *Scanner
	handler http.Handler
	md      *fakeMassdriver
	slack   *fakeSlack
	seen    []string
}

func newHarness(t *testing.T, layers map[string]string, attrKey string) *harness {
	t.Helper()
	md := &fakeMassdriver{layers: layers}
	mdSrv := httptest.NewServer(md)
	t.Cleanup(mdSrv.Close)
	slack := &fakeSlack{}
	slackSrv := httptest.NewServer(slack)
	t.Cleanup(slackSrv.Close)

	trivy, trivySeen := fakeTool(t, "trivy", trivyOutput, 0, parseTrivy)
	checkov, checkovSeen := fakeTool(t, "checkov", checkovOutput, 1, parseCheckov)

	s := New(&massdriver.Client{Endpoint: mdSrv.URL + "/api/v2", OrganizationID: "acme", APIKey: "md_test"}, Config{
		SlackWebhookURL: slackSrv.URL,
		Tools:           []Tool{trivy, checkov},
		FailSeverity:    "HIGH",
		AttributeKey:    attrKey,
		Timeout:         time.Minute,
	})
	return &harness{scanner: s, handler: automation.Handler(automationName, s, secret), md: md, slack: slack, seen: []string{trivySeen, checkovSeen}}
}

// drain scans every queued job in the test goroutine.
func (h *harness) drain() {
	for {
		select {
		case j := <-h.scanner.queue:
			h.scanner.process(j)
		default:
			return
		}
	}
}

var eventTime = time.Date(2026, 11, 24, 5, 54, 3, 0, time.UTC)

func TestScansTheVersionThatThePublishAnnounced(t *testing.T) {
	h := newHarness(t, map[string]string{"src/main.tf": mainTF, "massdriver.yaml": "name: aws-aurora\n"}, "scan")

	if code := post(t, h.handler, repoPublished("evt-1", "aws-aurora", bundleArtifactType, eventTime)); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	h.drain()

	for _, seen := range h.seen {
		b, err := os.ReadFile(seen)
		if err != nil || string(b) != mainTF {
			t.Fatalf("scanner did not see the pulled src/main.tf: %q, %v", b, err)
		}
	}

	wantSlack := ":x: *aws-aurora@1.2.3* failed the IaC scan (fails at HIGH or above): 0 critical, 1 high, 0 medium, 1 low, 1 unrated.\n" +
		"• HIGH `AWS-0086` src/main.tf:1 — S3 Access block should block public ACL (trivy)"
	if len(h.slack.texts) != 1 || h.slack.texts[0] != wantSlack {
		t.Fatalf("slack = %q\nwant  %q", h.slack.texts, wantSlack)
	}

	if len(h.md.updates) != 1 {
		t.Fatalf("updates = %v", h.md.updates)
	}
	u := h.md.updates[0]
	if u["org"] != "acme" || u["id"] != "aws-aurora" {
		t.Errorf("update target = %v", u)
	}
	if got := u["input"].(map[string]any)["attributes"]; got != `{"owner":"platform","scan":"failed"}` {
		t.Errorf("attributes = %v", got)
	}

	for _, line := range h.md.auth {
		if !strings.HasSuffix(line, " Bearer md_test") {
			t.Errorf("request without the service account token: %s", line)
		}
	}
}

func TestPassesWhenNoFindingReachesTheThreshold(t *testing.T) {
	h := newHarness(t, map[string]string{"src/main.tf": mainTF}, "scan")
	h.scanner.cfg.FailSeverity = "CRITICAL"

	post(t, h.handler, repoPublished("evt-1", "aws-aurora", bundleArtifactType, eventTime))
	h.drain()

	want := ":white_check_mark: *aws-aurora@1.2.3* passed the IaC scan (fails at CRITICAL or above): 0 critical, 1 high, 0 medium, 1 low, 1 unrated."
	if len(h.slack.texts) != 1 || h.slack.texts[0] != want {
		t.Fatalf("slack = %q", h.slack.texts)
	}
	if got := h.md.updates[0]["input"].(map[string]any)["attributes"]; got != `{"owner":"platform","scan":"passed"}` {
		t.Errorf("attributes = %v", got)
	}
}

func TestScansEachVersionOnce(t *testing.T) {
	h := newHarness(t, map[string]string{"src/main.tf": mainTF}, "")

	post(t, h.handler, repoPublished("evt-1", "aws-aurora", bundleArtifactType, eventTime))
	post(t, h.handler, repoPublished("evt-2", "aws-aurora", bundleArtifactType, eventTime))
	h.drain()

	if len(h.slack.texts) != 1 {
		t.Fatalf("slack posts = %d, want 1", len(h.slack.texts))
	}
	if len(h.md.updates) != 0 {
		t.Errorf("updates = %v, want none without SCAN_ATTRIBUTE_KEY", h.md.updates)
	}
}

func TestRefusesAFileNameOutsideTheBundle(t *testing.T) {
	h := newHarness(t, map[string]string{"../../etc/cron.d/x": "* * * * * root id"}, "scan")

	post(t, h.handler, repoPublished("evt-1", "aws-aurora", bundleArtifactType, eventTime))
	h.drain()

	if len(h.slack.texts) != 1 || !strings.HasPrefix(h.slack.texts[0], ":warning: *aws-aurora@1.2.3* could not be scanned: pull: layer sha256:") {
		t.Fatalf("slack = %q", h.slack.texts)
	}
	if len(h.md.updates) != 0 {
		t.Errorf("updates = %v, want none after a failed scan", h.md.updates)
	}
}

func TestIgnoresOtherEvents(t *testing.T) {
	h := newHarness(t, nil, "scan")

	other, _ := json.Marshal(map[string]any{"id": "evt-2", "type": "repo.created", "data": map[string]any{"entity": map[string]any{"name": "aws-aurora", "artifact_type": bundleArtifactType}}})
	for _, body := range [][]byte{
		repoPublished("evt-1", "aws-iam-role", "application/vnd.massdriver.resource-type.v1+json", eventTime),
		other,
	} {
		if code := post(t, h.handler, body); code != http.StatusNoContent {
			t.Fatalf("status = %d", code)
		}
	}
	if len(h.scanner.queue) != 0 {
		t.Fatalf("queued %d scans, want 0", len(h.scanner.queue))
	}
}

func TestFullQueueAsksMassdriverToRetry(t *testing.T) {
	h := newHarness(t, nil, "")
	for i := range queueSize {
		h.scanner.queue <- job{repo: "r", eventID: "queued-" + strconv.Itoa(i)}
	}
	if code := post(t, h.handler, repoPublished("evt-1", "aws-aurora", bundleArtifactType, eventTime)); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

func TestParseCheckovSingleFramework(t *testing.T) {
	fs, err := parseCheckov([]byte(`{"check_type":"terraform","results":{"failed_checks":[{"check_id":"CKV_AWS_1","check_name":"n","file_path":"/a.tf","severity":"critical","file_line_range":[4,9]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0] != (finding{Tool: "checkov", ID: "CKV_AWS_1", Title: "n", Severity: "CRITICAL", File: "a.tf", Line: 4}) {
		t.Fatalf("findings = %+v", fs)
	}
}
