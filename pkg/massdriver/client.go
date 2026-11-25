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
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.APIKey)

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("graphql: HTTP %d: %s", resp.StatusCode, b)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}
		return errors.New("graphql: " + strings.Join(msgs, "; "))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
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
