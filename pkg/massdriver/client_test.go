package massdriver

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// dropFirst closes the connection without a response on the first request, the way a load balancer that
// recycled an idle connection does, and answers every later request.
func dropFirst(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			return
		}
		w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestQueryIsRetriedAfterADroppedConnection(t *testing.T) {
	srv, calls := dropFirst(t)
	c := &Client{Endpoint: srv.URL, APIKey: "md_test"}
	var out struct{ OK bool }
	if err := c.Do(t.Context(), "query Groups { ok }", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || calls.Load() != 2 {
		t.Fatalf("ok=%v calls=%d, want true and 2", out.OK, calls.Load())
	}
}

func TestMutationIsNotRetried(t *testing.T) {
	srv, calls := dropFirst(t)
	c := &Client{Endpoint: srv.URL, APIKey: "md_test"}
	if err := c.Do(t.Context(), "mutation Create { ok }", nil, nil); err == nil {
		t.Fatal("want an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}
