package auditarchiver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

// Whitespace and key order differ from what encoding/json would produce, so a re-encoded body would not match.
const rawEvent = `{"specversion":"1.0",  "id":"4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2","type":"project.created",
 "source":"https://app.massdriver.cloud","subject":"mri:acme:::project:api","time":"2026-11-24T05:54:03.482913Z",
 "datacontenttype":"application/json","data":{"entity":{"name":"api","identifier":"api"},"attributes":{"md-project":"api"},"metadata":{},"actor":{"name":"Jane Doe"}}}`

func deliver(t *testing.T, h http.Handler) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(rawEvent))
	automation.SignRequest(req, secret, "4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2", []byte(rawEvent), time.Now())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

type captured struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// fake records every request and answers with the next status in statuses, then 200.
type fake struct {
	mu       sync.Mutex
	requests []captured
	statuses []int
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if strings.Contains(r.Header.Get("content-encoding"), "aws-chunked") {
		body = decodeChunked(body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, captured{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
	status := http.StatusOK
	if len(f.statuses) > 0 {
		status, f.statuses = f.statuses[0], f.statuses[1:]
	}
	f.mu.Unlock()
	w.WriteHeader(status)
}

func (f *fake) all() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.requests...)
}

// decodeChunked reads an aws-chunked body: "<hex size>[;ext]\r\n<data>\r\n" repeated, then trailers.
func decodeChunked(b []byte) []byte {
	var out []byte
	r := bufio.NewReader(bytes.NewReader(b))
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return out
		}
		size, err := strconv.ParseInt(strings.SplitN(strings.TrimSpace(line), ";", 2)[0], 16, 64)
		if err != nil || size == 0 {
			return out
		}
		chunk := make([]byte, size)
		_, _ = io.ReadFull(r, chunk)
		out = append(out, chunk...)
		_, _ = r.ReadString('\n')
	}
}

func testS3Sink(endpoint string, cfg S3Config) *S3Sink {
	cfg.Endpoint = endpoint
	s := NewS3Sink(cfg, aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDTEST", "secrettest", ""),
	})
	s.now = func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestArchivesTheRawEventToEverySink(t *testing.T) {
	s3fake, splunkFake, ddFake := &fake{}, &fake{}, &fake{statuses: []int{http.StatusAccepted}}
	s3srv, splunkSrv, ddSrv := httptest.NewServer(s3fake), httptest.NewServer(splunkFake), httptest.NewServer(ddFake)
	defer s3srv.Close()
	defer splunkSrv.Close()
	defer ddSrv.Close()

	h := automation.Handler(New([]Sink{
		testS3Sink(s3srv.URL, S3Config{Bucket: "audit", Prefix: "events", ObjectLock: &ObjectLock{Mode: types.ObjectLockModeCompliance, Days: 365}}),
		&SplunkSink{cfg: SplunkConfig{URL: splunkSrv.URL, token: "hec-token", Index: "massdriver", Source: "massdriver", Sourcetype: "massdriver:cloudevent"}, http: http.DefaultClient},
		&DatadogSink{cfg: DatadogConfig{URL: ddSrv.URL, apiKey: "dd-key", Service: "massdriver", Tags: []string{"env:prod", "team:platform"}}, http: http.DefaultClient},
	}), secret)

	if code := deliver(t, h); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}

	puts := s3fake.all()
	if len(puts) != 1 {
		t.Fatalf("s3 requests = %d, want 1", len(puts))
	}
	put := puts[0]
	if put.method != http.MethodPut || put.path != "/audit/events/2026/11/24/05/20261124T055403.482913Z-4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2.json" {
		t.Errorf("s3 request = %s %s", put.method, put.path)
	}
	if string(put.body) != rawEvent {
		t.Errorf("s3 body = %q", put.body)
	}
	for header, want := range map[string]string{
		"content-type":                        "application/cloudevents+json",
		"x-amz-meta-webhook-id":               "4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2",
		"x-amz-object-lock-mode":              "COMPLIANCE",
		"x-amz-object-lock-retain-until-date": "2027-11-25T12:00:00Z",
	} {
		if got := put.header.Get(header); got != want {
			t.Errorf("s3 %s = %q, want %q", header, got, want)
		}
	}
	if !strings.HasPrefix(put.header.Get("authorization"), "AWS4-HMAC-SHA256 Credential=AKIDTEST/") {
		t.Errorf("s3 authorization = %q", put.header.Get("authorization"))
	}

	hec := splunkFake.all()
	if len(hec) != 1 || hec[0].path != "/services/collector/event" || hec[0].header.Get("authorization") != "Splunk hec-token" {
		t.Fatalf("splunk requests = %+v", hec)
	}
	var hecBody map[string]any
	_ = json.Unmarshal(hec[0].body, &hecBody)
	if hecBody["event"] != rawEvent {
		t.Errorf("splunk event = %q", hecBody["event"])
	}
	if hecBody["time"] != 1795499643.482913 || hecBody["index"] != "massdriver" || hecBody["sourcetype"] != "massdriver:cloudevent" || hecBody["source"] != "massdriver" {
		t.Errorf("splunk payload = %v", hecBody)
	}
	fields, _ := hecBody["fields"].(map[string]any)
	if fields["webhook_id"] != "4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2" || fields["event_type"] != "project.created" {
		t.Errorf("splunk fields = %v", fields)
	}

	logs := ddFake.all()
	if len(logs) != 1 || logs[0].path != "/api/v2/logs" || logs[0].header.Get("dd-api-key") != "dd-key" {
		t.Fatalf("datadog requests = %+v", logs)
	}
	var entries []map[string]any
	_ = json.Unmarshal(logs[0].body, &entries)
	if len(entries) != 1 {
		t.Fatalf("datadog entries = %v", entries)
	}
	e := entries[0]
	if e["message"] != rawEvent {
		t.Errorf("datadog message = %q", e["message"])
	}
	if e["ddsource"] != "massdriver" || e["service"] != "massdriver" || e["ddtags"] != "env:prod,team:platform" || e["webhook_id"] != "4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2" {
		t.Errorf("datadog entry = %v", e)
	}
}

func TestFailsUntilEverySinkWritesAndRetriesOnlyTheFailedSink(t *testing.T) {
	s3fake, ddFake := &fake{}, &fake{statuses: []int{http.StatusInternalServerError, http.StatusAccepted}}
	s3srv, ddSrv := httptest.NewServer(s3fake), httptest.NewServer(ddFake)
	defer s3srv.Close()
	defer ddSrv.Close()

	h := automation.Handler(New([]Sink{
		testS3Sink(s3srv.URL, S3Config{Bucket: "audit", Prefix: "md/events"}),
		&DatadogSink{cfg: DatadogConfig{URL: ddSrv.URL, apiKey: "dd-key", Service: "massdriver"}, http: http.DefaultClient},
	}), secret)

	if code := deliver(t, h); code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", code)
	}
	if code := deliver(t, h); code != http.StatusNoContent {
		t.Fatalf("second status = %d, want 204", code)
	}
	if code := deliver(t, h); code != http.StatusNoContent {
		t.Fatalf("third status = %d, want 204", code)
	}

	puts := s3fake.all()
	if len(puts) != 1 || puts[0].path != "/audit/md/events/2026/11/24/05/20261124T055403.482913Z-4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2.json" {
		t.Fatalf("s3 requests = %+v, want one PUT", puts)
	}
	if got := puts[0].header.Get("x-amz-object-lock-mode"); got != "" {
		t.Errorf("s3 object lock mode = %q, want none", got)
	}
	if n := len(ddFake.all()); n != 2 {
		t.Errorf("datadog requests = %d, want 2", n)
	}
}

func TestConfigDefaults(t *testing.T) {
	var c Config
	err := automation.DecodeConfig([]byte(`
s3:
  bucket: audit
  object_lock: {mode: governance, days: 30}
splunk:
  url: https://splunk.example.com:8088/
datadog:
  site: datadoghq.eu
`), &c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.S3.Prefix != "events" || c.S3.ObjectLock.Mode != types.ObjectLockModeGovernance || c.S3.ObjectLock.Days != 30 {
		t.Errorf("s3 = %+v", *c.S3)
	}
	if c.Splunk.URL != "https://splunk.example.com:8088" || c.Splunk.TokenEnv != "SPLUNK_HEC_TOKEN" || c.Splunk.Sourcetype != "massdriver:cloudevent" {
		t.Errorf("splunk = %+v", *c.Splunk)
	}
	if c.Datadog.URL != "https://http-intake.logs.datadoghq.eu" || c.Datadog.APIKeyEnv != "DD_API_KEY" || c.Datadog.Service != "massdriver" {
		t.Errorf("datadog = %+v", *c.Datadog)
	}
}

func TestStartReadsTokensFromTheNamedEnvVars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("splunk:\n  url: https://splunk.example.com:8088\n  token_env: HEC_TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", path)
	t.Setenv("HEC_TOKEN", "")
	if _, err := Definition.New(); err == nil || err.Error() != "missing required env vars: [HEC_TOKEN]" {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("HEC_TOKEN", "hec-token")
	a, err := Definition.New()
	if err != nil {
		t.Fatal(err)
	}
	sink := a.(*Archiver).sinks[0].(*SplunkSink)
	if sink.cfg.token != "hec-token" {
		t.Fatalf("token = %q", sink.cfg.token)
	}
}

func TestStartRefusesBadConfig(t *testing.T) {
	cases := map[string]string{
		"":         "no sink configured",
		"s3: {}\n": "s3.bucket is required",
		"s3: {bucket: a, object_lock: {mode: forever, days: 1}}\n": "s3.object_lock.mode must be GOVERNANCE or COMPLIANCE",
		"s3: {bucket: a, object_lock: {mode: COMPLIANCE}}\n":       "s3.object_lock.days must be 1 or more",
		"splunk: {index: main}\n":                                  "splunk.url is required",
		"datadog: {site: datadoghq.eu, tags: env:prod}\n":          "line 1: a str value is the wrong type here",
		"s3: {bucket: a, lock_days: 3}\n":                          "field lock_days not found",
	}
	for body, want := range cases {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONFIG_FILE", path)
		_, err := Definition.New()
		var ce *automation.ConfigError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want a config error with %q", body, err, want)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var c Config
	if err := automation.LoadConfigFile("config.example.yaml", &c); err != nil {
		t.Fatal(err)
	}
	if c.S3 == nil || c.Splunk == nil || c.Datadog == nil {
		t.Fatalf("config = %+v", c)
	}
}

func TestDiskSinkWritesTheRawEventOncePerID(t *testing.T) {
	dir := t.TempDir()
	sink := &DiskSink{dir: dir}
	ev := automation.Event{ID: "4b0b5a8e", Time: time.Date(2026, 11, 24, 5, 54, 3, 482913000, time.UTC), Raw: []byte(`{"id":"4b0b5a8e"}`)}

	if err := sink.Write(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(t.Context(), ev); err != nil {
		t.Fatal(err)
	}

	hour := filepath.Join(dir, "2026", "11", "24", "05")
	entries, err := os.ReadDir(hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "20261124T055403.482913Z-4b0b5a8e.json" {
		t.Fatalf("files = %v", entries)
	}
	got, _ := os.ReadFile(filepath.Join(hour, entries[0].Name()))
	if string(got) != `{"id":"4b0b5a8e"}` {
		t.Fatalf("body = %s", got)
	}
}
