package massdriver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func logsServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}
}

func TestDeploymentLogsWritesEachBatch(t *testing.T) {
	c := logsServer(t, `{"data":{"deployment":{"logs":[{"message":"one\ntwo"},{"message":"three"}],"id":"d"}}}`)
	var b strings.Builder
	n, truncated, err := c.DeploymentLogs(t.Context(), "d", &b, 1<<20)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if b.String() != "one\ntwo\nthree\n" || n != int64(b.Len()) {
		t.Fatalf("got %q (%d bytes)", b.String(), n)
	}
}

func TestDeploymentLogsStopsAtTheLimit(t *testing.T) {
	c := logsServer(t, `{"data":{"deployment":{"logs":[{"message":"0123456789"},{"message":"abcdef"}]}}}`)
	var b strings.Builder
	n, truncated, err := c.DeploymentLogs(t.Context(), "d", &b, 13)
	if err != nil || !truncated || n != 13 || b.String() != "0123456789\nab" {
		t.Fatalf("got %q n=%d truncated=%v err=%v", b.String(), n, truncated, err)
	}
}

func TestDeploymentLogsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"graphql error after data": {`{"data":{"deployment":null},"errors":[{"message":"not authorized"}]}`, "graphql: not authorized"},
		"graphql error first":      {`{"errors":[{"message":"boom"}],"data":null}`, "graphql: boom"},
		"not found":                {`{"data":{"deployment":null}}`, ErrDeploymentNotFound.Error()},
		"cut response":             {`{"data":{"deployment":{"logs":[{"message":"x"}`, "deployment logs"},
	} {
		t.Run(name, func(t *testing.T) {
			c := logsServer(t, tc.body)
			_, _, err := c.DeploymentLogs(t.Context(), "d", &strings.Builder{}, 1<<20)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDeploymentLogsOfADeploymentWithoutLogs(t *testing.T) {
	c := logsServer(t, `{"data":{"deployment":{"logs":[]}}}`)
	n, _, err := c.DeploymentLogs(t.Context(), "d", &strings.Builder{}, 1<<20)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if errors.Is(err, ErrDeploymentNotFound) {
		t.Fatal("a deployment without logs exists")
	}
}

func TestStripANSI(t *testing.T) {
	if got := StripANSI("\x1b[1mPlan:\x1b[0m 1 to add"); got != "Plan: 1 to add" {
		t.Fatalf("got %q", got)
	}
}
