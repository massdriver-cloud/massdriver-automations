package stashsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// PrometheusQuery is a PromQL expression that returns one number: a scalar, or a vector with one series.
type PrometheusQuery struct {
	Query string `yaml:"query"`
}

func (*PrometheusQuery) source() string { return "prometheus" }

// Prometheus runs instant queries with the HTTP API, `/api/v1/query`.
type Prometheus struct {
	url      string
	token    string
	username string
	password string
	http     *http.Client
}

// NewPrometheus returns a Prometheus source. A token is sent as a bearer token. A username is sent with the
// password as basic auth.
func NewPrometheus(baseURL, token, username, password string, hc *http.Client) *Prometheus {
	return &Prometheus{url: strings.TrimRight(baseURL, "/") + "/api/v1/query", token: token, username: username,
		password: password, http: hc}
}

// Query runs an instant query. An empty result, or NaN, is ErrNoData.
func (p *Prometheus) Query(ctx context.Context, query Query) (float64, error) {
	q, ok := query.(*PrometheusQuery)
	if !ok {
		return 0, fmt.Errorf("prometheus cannot run a %s query", query.source())
	}
	form := url.Values{"query": {q.Query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	switch {
	case p.token != "":
		req.Header.Set("Authorization", "Bearer "+p.token)
	case p.username != "":
		req.SetBasicAuth(p.username, p.password)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, automation.RedactURL(err)
	}
	defer resp.Body.Close()

	var body struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return 0, fmt.Errorf("prometheus: HTTP %d", resp.StatusCode)
	}
	if body.Status != "success" {
		return 0, fmt.Errorf("prometheus: HTTP %d: %s: %s", resp.StatusCode, body.ErrorType, body.Error)
	}

	var sample []any
	switch body.Data.ResultType {
	case "scalar":
		if err := json.Unmarshal(body.Data.Result, &sample); err != nil {
			return 0, err
		}
	case "vector":
		var series []struct {
			Value []any `json:"value"`
		}
		if err := json.Unmarshal(body.Data.Result, &series); err != nil {
			return 0, err
		}
		if len(series) == 0 {
			return 0, ErrNoData
		}
		if len(series) > 1 {
			return 0, fmt.Errorf("prometheus: the query returned %d series; aggregate it to one, such as with sum()", len(series))
		}
		sample = series[0].Value
	default:
		return 0, fmt.Errorf("prometheus: the query returned a %s; it must return a scalar or one series", body.Data.ResultType)
	}
	if len(sample) != 2 {
		return 0, errors.New("prometheus: a sample must have a time and a value")
	}
	s, _ := sample[1].(string)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, errors.New("prometheus: the value is not a number")
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, ErrNoData
	}
	return v, nil
}
