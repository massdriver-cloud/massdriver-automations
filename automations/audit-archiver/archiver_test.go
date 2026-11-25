package auditarchiver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		testS3Sink(s3srv.URL, S3Config{Bucket: "audit", Prefix: "events", LockMode: types.ObjectLockModeCompliance, LockDays: 365}),
		&SplunkSink{cfg: SplunkConfig{URL: splunkSrv.URL + "/services/collector/event", Token: "hec-token", Index: "massdriver", Source: "massdriver", Sourcetype: "massdriver:cloudevent"}, http: http.DefaultClient},
		&DatadogSink{cfg: DatadogConfig{URL: ddSrv.URL + "/api/v2/logs", APIKey: "dd-key", Service: "massdriver", Tags: "env:prod"}, http: http.DefaultClient},
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
	if e["ddsource"] != "massdriver" || e["service"] != "massdriver" || e["ddtags"] != "env:prod" || e["webhook_id"] != "4b0b5a8e-0f6c-4a53-9d0c-5a1f4ad5c1c2" {
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
		&DatadogSink{cfg: DatadogConfig{URL: ddSrv.URL + "/api/v2/logs", APIKey: "dd-key", Service: "massdriver"}, http: http.DefaultClient},
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

func TestLoadConfig(t *testing.T) {
	t.Setenv("ARCHIVE_S3_BUCKET", "audit")
	t.Setenv("ARCHIVE_S3_OBJECT_LOCK_MODE", "governance")
	t.Setenv("ARCHIVE_S3_OBJECT_LOCK_DAYS", "30")
	t.Setenv("SPLUNK_HEC_URL", "https://splunk.example.com:8088/")
	t.Setenv("SPLUNK_HEC_TOKEN", "hec-token")
	t.Setenv("DD_API_KEY", "dd-key")
	t.Setenv("DD_SITE", "datadoghq.eu")

	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.s3.Prefix != "events" || c.s3.LockMode != types.ObjectLockModeGovernance || c.s3.LockDays != 30 {
		t.Errorf("s3 = %+v", *c.s3)
	}
	if c.splunk.URL != "https://splunk.example.com:8088/services/collector/event" || c.splunk.Sourcetype != "massdriver:cloudevent" {
		t.Errorf("splunk = %+v", *c.splunk)
	}
	if c.datadog.URL != "https://http-intake.logs.datadoghq.eu/api/v2/logs" {
		t.Errorf("datadog url = %q", c.datadog.URL)
	}

	t.Setenv("ARCHIVE_S3_OBJECT_LOCK_DAYS", "")
	if _, err := loadConfig(); err == nil || err.Error() != "set both ARCHIVE_S3_OBJECT_LOCK_MODE and ARCHIVE_S3_OBJECT_LOCK_DAYS, or neither" {
		t.Errorf("err = %v", err)
	}
}
