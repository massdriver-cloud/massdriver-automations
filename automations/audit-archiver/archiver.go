// Package auditarchiver stores every Massdriver event, unchanged, in S3, Splunk, Datadog, or all of them.
package auditarchiver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

const (
	// Massdriver waits 10 seconds; an answer after that counts as a failure anyway.
	writeTimeout = 8 * time.Second
	keyTime      = "20060102T150405.000000Z"
)

// Definition registers the automation. No event types: the destination selects every event.
var Definition = automation.Definition{
	Name:        "audit-archiver",
	Description: "Archives every event, unchanged, in S3, Splunk, or Datadog.",
	New: func() (automation.Automation, error) {
		cfg, err := loadConfig()
		if err != nil {
			return nil, err
		}
		var sinks []Sink
		if cfg.s3 != nil {
			awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
			if err != nil {
				return nil, err
			}
			sinks = append(sinks, NewS3Sink(*cfg.s3, awsCfg))
		}
		if cfg.splunk != nil {
			sinks = append(sinks, &SplunkSink{cfg: *cfg.splunk, http: http.DefaultClient})
		}
		if cfg.datadog != nil {
			sinks = append(sinks, &DatadogSink{cfg: *cfg.datadog, http: http.DefaultClient})
		}
		return New(sinks), nil
	},
}

// S3Config selects the bucket and the retention of each object.
type S3Config struct {
	Bucket   string
	Prefix   string
	Endpoint string
	LockMode types.ObjectLockMode
	LockDays int
}

// SplunkConfig is an HTTP Event Collector.
type SplunkConfig struct {
	URL        string
	Token      string
	Index      string
	Source     string
	Sourcetype string
}

// DatadogConfig is a Datadog Logs intake.
type DatadogConfig struct {
	URL     string
	APIKey  string
	Service string
	Tags    string
}

type config struct {
	s3      *S3Config
	splunk  *SplunkConfig
	datadog *DatadogConfig
}

func loadConfig() (config, error) {
	var c config

	if bucket := os.Getenv("ARCHIVE_S3_BUCKET"); bucket != "" {
		sc := &S3Config{
			Bucket:   bucket,
			Prefix:   strings.Trim(automation.Env("ARCHIVE_S3_PREFIX", "events"), "/"),
			Endpoint: os.Getenv("ARCHIVE_S3_ENDPOINT"),
		}
		mode, days := os.Getenv("ARCHIVE_S3_OBJECT_LOCK_MODE"), os.Getenv("ARCHIVE_S3_OBJECT_LOCK_DAYS")
		if (mode == "") != (days == "") {
			return c, errors.New("set both ARCHIVE_S3_OBJECT_LOCK_MODE and ARCHIVE_S3_OBJECT_LOCK_DAYS, or neither")
		}
		if mode != "" {
			sc.LockMode = types.ObjectLockMode(strings.ToUpper(mode))
			if sc.LockMode != types.ObjectLockModeGovernance && sc.LockMode != types.ObjectLockModeCompliance {
				return c, fmt.Errorf("ARCHIVE_S3_OBJECT_LOCK_MODE must be GOVERNANCE or COMPLIANCE, got %q", mode)
			}
			n, err := strconv.Atoi(days)
			if err != nil || n < 1 {
				return c, fmt.Errorf("ARCHIVE_S3_OBJECT_LOCK_DAYS must be a whole number of days, 1 or more, got %q", days)
			}
			sc.LockDays = n
		}
		c.s3 = sc
	}

	if u := os.Getenv("SPLUNK_HEC_URL"); u != "" {
		token := os.Getenv("SPLUNK_HEC_TOKEN")
		if token == "" {
			return c, errors.New("SPLUNK_HEC_URL is set, but SPLUNK_HEC_TOKEN is not")
		}
		c.splunk = &SplunkConfig{
			URL:        strings.TrimRight(u, "/") + "/services/collector/event",
			Token:      token,
			Index:      os.Getenv("SPLUNK_INDEX"),
			Source:     automation.Env("SPLUNK_SOURCE", "massdriver"),
			Sourcetype: automation.Env("SPLUNK_SOURCETYPE", "massdriver:cloudevent"),
		}
	}

	if key := os.Getenv("DD_API_KEY"); key != "" {
		base := automation.Env("DD_LOGS_URL", "https://http-intake.logs."+automation.Env("DD_SITE", "datadoghq.com"))
		c.datadog = &DatadogConfig{
			URL:     strings.TrimRight(base, "/") + "/api/v2/logs",
			APIKey:  key,
			Service: automation.Env("DD_SERVICE", "massdriver"),
			Tags:    os.Getenv("DD_TAGS"),
		}
	}

	if c.s3 == nil && c.splunk == nil && c.datadog == nil {
		return c, errors.New("no sink configured: set ARCHIVE_S3_BUCKET, SPLUNK_HEC_URL, or DD_API_KEY")
	}
	return c, nil
}

// Sink stores one event.
type Sink interface {
	Name() string
	Write(ctx context.Context, ev automation.Event) error
}

// Archiver writes each event to every sink.
type Archiver struct {
	sinks []Sink

	mu sync.Mutex
	// Sinks that already hold an event, so a retry after a partial failure writes only to the others.
	written map[string]*progress
}

type progress struct {
	first time.Time
	sinks map[string]bool
}

// New returns the automation.
func New(sinks []Sink) *Archiver {
	return &Archiver{sinks: sinks, written: map[string]*progress{}}
}

// Handle returns only after every sink holds the event, so a failed write makes Massdriver retry it.
func (a *Archiver) Handle(ctx context.Context, ev automation.Event) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	pending := a.pending(ev.ID)
	errs := make([]error, len(pending))
	var wg sync.WaitGroup
	for i, s := range pending {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Write(ctx, ev); err != nil {
				errs[i] = fmt.Errorf("%s: %w", s.Name(), err)
				return
			}
			a.markWritten(ev.ID, s.Name())
		}()
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return err
	}
	automation.Logger(ctx).Info("event archived", "sinks", len(pending))
	return nil
}

func (a *Archiver) pending(id string) []Sink {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, p := range a.written {
		if now.Sub(p.first) > time.Hour {
			delete(a.written, k)
		}
	}
	p := a.written[id]
	var out []Sink
	for _, s := range a.sinks {
		if p == nil || !p.sinks[s.Name()] {
			out = append(out, s)
		}
	}
	return out
}

func (a *Archiver) markWritten(id, sink string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.written[id]
	if p == nil {
		p = &progress{first: time.Now(), sinks: map[string]bool{}}
		a.written[id] = p
	}
	p.sinks[sink] = true
}

// --- S3 --------------------------------------------------------------------

// S3Sink puts each event in one object.
type S3Sink struct {
	cfg    S3Config
	client *s3.Client
	now    func() time.Time
}

// NewS3Sink returns a sink that writes with the credentials and region of awsCfg.
func NewS3Sink(cfg S3Config, awsCfg aws.Config) *S3Sink {
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			// Most S3-compatible stores do not serve bucket subdomains.
			o.UsePathStyle = true
		}
	})
	return &S3Sink{cfg: cfg, client: client, now: time.Now}
}

// Name is "s3".
func (s *S3Sink) Name() string { return "s3" }

// The key depends only on the event, so a redelivered event lands on the same object.
func (s *S3Sink) key(ev automation.Event) string {
	t := ev.Time.UTC()
	return fmt.Sprintf("%s/%s/%s-%s.json", s.cfg.Prefix, t.Format("2006/01/02/15"), t.Format(keyTime), ev.ID)
}

// Write puts the event body in the bucket.
func (s *S3Sink) Write(ctx context.Context, ev automation.Event) error {
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(s.key(ev)),
		Body:        bytes.NewReader(ev.Raw),
		ContentType: aws.String("application/cloudevents+json"),
		Metadata:    map[string]string{"webhook-id": ev.ID},
	}
	if s.cfg.LockMode != "" {
		in.ObjectLockMode = s.cfg.LockMode
		in.ObjectLockRetainUntilDate = aws.Time(s.now().UTC().AddDate(0, 0, s.cfg.LockDays))
	}
	_, err := s.client.PutObject(ctx, in)
	return err
}

// --- Splunk ----------------------------------------------------------------

// SplunkSink sends each event to an HTTP Event Collector.
type SplunkSink struct {
	cfg  SplunkConfig
	http *http.Client
}

// Name is "splunk".
func (s *SplunkSink) Name() string { return "splunk" }

// Write sends the event to the collector.
func (s *SplunkSink) Write(ctx context.Context, ev automation.Event) error {
	payload := map[string]any{
		"time":       float64(ev.Time.UnixMicro()) / 1e6,
		"source":     s.cfg.Source,
		"sourcetype": s.cfg.Sourcetype,
		// A string, not an object, so Splunk keeps the body exactly as Massdriver sent it.
		"event": string(ev.Raw),
		"fields": map[string]string{
			"webhook_id": ev.ID,
			"event_type": ev.Type,
		},
	}
	if s.cfg.Index != "" {
		payload["index"] = s.cfg.Index
	}
	return post(ctx, s.http, s.cfg.URL, map[string]string{"authorization": "Splunk " + s.cfg.Token}, payload)
}

// --- Datadog ---------------------------------------------------------------

// DatadogSink sends each event to the Datadog Logs intake.
type DatadogSink struct {
	cfg  DatadogConfig
	http *http.Client
}

// Name is "datadog".
func (d *DatadogSink) Name() string { return "datadog" }

// Write sends the event as one log.
func (d *DatadogSink) Write(ctx context.Context, ev automation.Event) error {
	entry := map[string]any{
		"ddsource":   "massdriver",
		"service":    d.cfg.Service,
		"message":    string(ev.Raw),
		"webhook_id": ev.ID,
	}
	if d.cfg.Tags != "" {
		entry["ddtags"] = d.cfg.Tags
	}
	return post(ctx, d.http, d.cfg.URL, map[string]string{"dd-api-key": d.cfg.APIKey}, []any{entry})
}

func post(ctx context.Context, c *http.Client, url string, headers map[string]string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return automation.RedactURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}
