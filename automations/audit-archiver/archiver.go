// Package auditarchiver stores every Massdriver event, unchanged, in S3, Splunk, Datadog, or all of them.
package auditarchiver

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		if err := cfg.readSecrets(); err != nil {
			return nil, err
		}
		var sinks []Sink
		if cfg.Disk != nil {
			sinks = append(sinks, &DiskSink{dir: cfg.Disk.Dir})
		}
		if cfg.S3 != nil {
			var opts []func(*awsconfig.LoadOptions) error
			if cfg.S3.Region != "" {
				opts = append(opts, awsconfig.WithRegion(cfg.S3.Region))
			}
			awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
			if err != nil {
				return nil, err
			}
			sinks = append(sinks, NewS3Sink(*cfg.S3, awsCfg))
		}
		if cfg.Splunk != nil {
			sinks = append(sinks, &SplunkSink{cfg: *cfg.Splunk, http: http.DefaultClient})
		}
		if cfg.Datadog != nil {
			sinks = append(sinks, &DatadogSink{cfg: *cfg.Datadog, http: http.DefaultClient})
		}
		return New(sinks), nil
	},
}

// Config is the config file of audit-archiver. Each section that is set turns on its sink.
type Config struct {
	Disk    *DiskConfig    `yaml:"disk"`
	S3      *S3Config      `yaml:"s3"`
	Splunk  *SplunkConfig  `yaml:"splunk"`
	Datadog *DatadogConfig `yaml:"datadog"`
}

// DiskConfig writes events under a directory, with the same layout as S3 keys.
type DiskConfig struct {
	Dir string `yaml:"dir"`
}

// S3Config selects the bucket and the retention of each object.
type S3Config struct {
	Bucket string `yaml:"bucket"`
	// Region replaces AWS_REGION for the bucket.
	Region     string      `yaml:"region"`
	Prefix     string      `yaml:"prefix"`
	Endpoint   string      `yaml:"endpoint"`
	ObjectLock *ObjectLock `yaml:"object_lock"`
}

// ObjectLock is the retention that each object gets.
type ObjectLock struct {
	Mode types.ObjectLockMode `yaml:"mode"`
	Days int                  `yaml:"days"`
}

// SplunkConfig is an HTTP Event Collector.
type SplunkConfig struct {
	URL        string `yaml:"url"`
	TokenEnv   string `yaml:"token_env"`
	Index      string `yaml:"index"`
	Source     string `yaml:"source"`
	Sourcetype string `yaml:"sourcetype"`

	token string
}

// DatadogConfig is a Datadog Logs intake.
type DatadogConfig struct {
	Site      string   `yaml:"site"`
	URL       string   `yaml:"url"`
	APIKeyEnv string   `yaml:"api_key_env"`
	Service   string   `yaml:"service"`
	Tags      []string `yaml:"tags"`

	apiKey string
}

// Validate checks the sinks and fills their defaults.
func (c *Config) Validate() error {
	if c.Disk == nil && c.S3 == nil && c.Splunk == nil && c.Datadog == nil {
		return errors.New("no sink configured: set disk, s3, splunk, datadog, or more than one")
	}
	if d := c.Disk; d != nil && d.Dir == "" {
		return errors.New("disk.dir is required")
	}
	if s := c.S3; s != nil {
		if s.Bucket == "" {
			return errors.New("s3.bucket is required")
		}
		s.Prefix = strings.Trim(s.Prefix, "/")
		if s.Prefix == "" {
			s.Prefix = "events"
		}
		if l := s.ObjectLock; l != nil {
			l.Mode = types.ObjectLockMode(strings.ToUpper(string(l.Mode)))
			if l.Mode != types.ObjectLockModeGovernance && l.Mode != types.ObjectLockModeCompliance {
				return errors.New("s3.object_lock.mode must be GOVERNANCE or COMPLIANCE")
			}
			if l.Days < 1 {
				return errors.New("s3.object_lock.days must be 1 or more")
			}
		}
	}
	if s := c.Splunk; s != nil {
		if s.URL == "" {
			return errors.New("splunk.url is required")
		}
		s.URL = strings.TrimRight(s.URL, "/")
		s.TokenEnv = cmp.Or(s.TokenEnv, "SPLUNK_HEC_TOKEN")
		s.Source = cmp.Or(s.Source, "massdriver")
		s.Sourcetype = cmp.Or(s.Sourcetype, "massdriver:cloudevent")
	}
	if d := c.Datadog; d != nil {
		d.Site = cmp.Or(d.Site, "datadoghq.com")
		d.URL = strings.TrimRight(cmp.Or(d.URL, "https://http-intake.logs."+d.Site), "/")
		d.APIKeyEnv = cmp.Or(d.APIKeyEnv, "DD_API_KEY")
		d.Service = cmp.Or(d.Service, "massdriver")
	}
	return nil
}

// readSecrets reads the tokens from the environment variables that the config names.
func (c *Config) readSecrets() error {
	var names []string
	if c.Splunk != nil {
		names = append(names, c.Splunk.TokenEnv)
	}
	if c.Datadog != nil {
		names = append(names, c.Datadog.APIKeyEnv)
	}
	v, err := automation.RequireEnv(names...)
	if err != nil {
		return err
	}
	if c.Splunk != nil {
		c.Splunk.token = v[c.Splunk.TokenEnv]
	}
	if c.Datadog != nil {
		c.Datadog.apiKey = v[c.Datadog.APIKeyEnv]
	}
	return nil
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
// DiskSink writes each event to a file.
type DiskSink struct {
	dir string
}

// Name names the sink in logs.
func (d *DiskSink) Name() string { return "disk" }

// Write stores the event body at <dir>/YYYY/MM/DD/HH/<time>-<id>.json.
// The path depends only on the event, so a redelivered event overwrites the same file. The rename makes a
// crash leave either the old file or the new one, never part of one.
func (d *DiskSink) Write(_ context.Context, ev automation.Event) error {
	t := ev.Time.UTC()
	dir := filepath.Join(d.dir, t.Format("2006/01/02/15"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(ev.Raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, fmt.Sprintf("%s-%s.json", t.Format(keyTime), filepath.Base(ev.ID))))
}

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
	if l := s.cfg.ObjectLock; l != nil {
		in.ObjectLockMode = l.Mode
		in.ObjectLockRetainUntilDate = aws.Time(s.now().UTC().AddDate(0, 0, l.Days))
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
	return post(ctx, s.http, s.cfg.URL+"/services/collector/event", map[string]string{"authorization": "Splunk " + s.cfg.token}, payload)
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
	if len(d.cfg.Tags) > 0 {
		entry["ddtags"] = strings.Join(d.cfg.Tags, ",")
	}
	return post(ctx, d.http, d.cfg.URL+"/api/v2/logs", map[string]string{"dd-api-key": d.cfg.apiKey}, []any{entry})
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
