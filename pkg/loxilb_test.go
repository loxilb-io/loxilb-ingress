package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
)

func testLoxiClient(t *testing.T, srv *httptest.Server) *loxiapi.LoxiClient {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := loxiapi.NewRESTClient(u, "netlox", "v1", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return &loxiapi.LoxiClient{RestClient: rc}
}

func TestWaitForLoxiLBReturnsOnceLoxilbAnswers(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// loxilb isn't up for the first requests: drop the connection without an answer.
		if calls.Add(1) <= 3 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		// loxilb answers 404 on the API root; any answer means it is up.
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !WaitForLoxiLB(ctx, testLoxiClient(t, srv), 10*time.Millisecond) {
		t.Fatal("WaitForLoxiLB gave up although loxilb answered")
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("requests = %d, want 4", got)
	}
}

func TestWaitForLoxiLBGivesUpWhenContextEnds(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	client := testLoxiClient(t, srv)
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if WaitForLoxiLB(ctx, client, 10*time.Millisecond) {
		t.Fatal("WaitForLoxiLB reported loxilb up while it was down")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("WaitForLoxiLB took %v after the context ended", elapsed)
	}
}
