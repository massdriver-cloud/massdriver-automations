// Package massdriver is a small client for the Massdriver V2 GraphQL API.
package massdriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// DefaultURL is Massdriver Cloud. Self-hosted installations set MASSDRIVER_URL.
const DefaultURL = "https://api.massdriver.cloud"

// Client calls the V2 GraphQL API as a service account.
type Client struct {
	// Endpoint is the full GraphQL URL, `<MASSDRIVER_URL>/api/v2`.
	Endpoint       string
	OrganizationID string
	APIKey         string
	HTTP           *http.Client
}

// FromEnv builds a Client from MASSDRIVER_URL, MASSDRIVER_ORGANIZATION_ID, and MASSDRIVER_API_KEY.
func FromEnv() (*Client, error) {
	v, err := automation.RequireEnv("MASSDRIVER_ORGANIZATION_ID", "MASSDRIVER_API_KEY")
	if err != nil {
		return nil, err
	}
	return &Client{
		Endpoint:       strings.TrimRight(automation.Env("MASSDRIVER_URL", DefaultURL), "/") + "/api/v2",
		OrganizationID: v["MASSDRIVER_ORGANIZATION_ID"],
		APIKey:         v["MASSDRIVER_API_KEY"],
	}, nil
}

// Do runs one GraphQL operation and decodes `data` into out. GraphQL errors become a Go error.
//
// `Map` inputs are JSON-encoded strings: pass them through JSONString.
func (c *Client) Do(ctx context.Context, query string, vars map[string]any, out any) error {
	resp, err := c.post(ctx, query, vars)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors graphQLErrors   `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if err := envelope.Errors.err(); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

type graphQLErrors []struct {
	Message string `json:"message"`
}

func (e graphQLErrors) err() error {
	if len(e) == 0 {
		return nil
	}
	msgs := make([]string, len(e))
	for i, m := range e {
		msgs[i] = m.Message
	}
	return errors.New("graphql: " + strings.Join(msgs, "; "))
}

// post sends one operation and returns the 200 response, for the caller to read and close.
func (c *Client) post(ctx context.Context, query string, vars map[string]any) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}
	opType, opName := operation(query)
	spanName := "graphql"
	if opName != "" {
		spanName += " " + opName
	}
	spanCtx := automation.WithClientSpan(ctx, spanName,
		semconv.GraphQLOperationTypeKey.String(opType), semconv.GraphQLOperationName(opName))
	req, err := http.NewRequestWithContext(spanCtx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.APIKey)

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = defaultHTTP
	}
	return c.send(ctx, httpClient, req, body, opType == "query")
}

// retryDelays are the waits between attempts of a query. A dropped connection (unexpected EOF, reset) or a
// gateway error from a load balancer is usually gone on the next attempt.
var retryDelays = []time.Duration{250 * time.Millisecond, time.Second}

// send posts the request. Only queries are retried: a mutation that failed after it was sent may have run.
func (c *Client) send(ctx context.Context, hc *http.Client, req *http.Request, body []byte, retry bool) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := hc.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		transient := err != nil && ctx.Err() == nil
		if err == nil {
			transient = resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable ||
				resp.StatusCode == http.StatusGatewayTimeout
		}
		if !retry || !transient || attempt >= len(retryDelays) {
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("graphql: HTTP %d: %s", resp.StatusCode, b)
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryDelays[attempt]):
		}
	}
}

var defaultHTTP = automation.HTTPClient(0)

// operation returns the type (query, mutation, or subscription) and name of the first operation in doc. A
// shorthand `{ ... }` is an unnamed query.
func operation(doc string) (opType, name string) {
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "{") {
			return "query", ""
		}
		if i := strings.IndexAny(line, "({@"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return "", ""
		}
		switch fields[0] {
		case "query", "mutation", "subscription":
			if len(fields) > 1 {
				return fields[0], fields[1]
			}
			return fields[0], ""
		}
		return "", ""
	}
	return "", ""
}

// JSONString encodes v for a `Map` input, which the V2 API takes as a JSON string.
func JSONString(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// Payload is the shape of every V2 mutation result.
type Payload[T any] struct {
	Successful bool `json:"successful"`
	Messages   []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"messages"`
	Result T `json:"result"`
}

// Err returns the validation messages of an unsuccessful payload as an error.
func (p Payload[T]) Err() error {
	if p.Successful {
		return nil
	}
	parts := make([]string, len(p.Messages))
	for i, m := range p.Messages {
		parts[i] = m.Field + ": " + m.Message
	}
	return errors.New(strings.Join(parts, "; "))
}

// DefaultAppURL is the Massdriver Cloud UI.
const DefaultAppURL = "https://app.massdriver.cloud"

// AppConfig is the `massdriver` section of an automation's config: where links in messages point.
type AppConfig struct {
	AppURL string `yaml:"app_url"`
}

// URL returns the UI URL without a trailing slash, or DefaultAppURL when app_url is not set.
func (c AppConfig) URL() string {
	if c.AppURL == "" {
		return DefaultAppURL
	}
	return strings.TrimRight(c.AppURL, "/")
}
