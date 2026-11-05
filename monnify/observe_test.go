package monnify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// What the server's provider health is built from: every request reports how it went.
func TestEveryRequestIsReportedToTheObserver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "oops", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "s", "CC")
	var statuses []int
	c.Observe = func(op string, status int, err error, took time.Duration) {
		if op == "" {
			t.Error("a call without a name")
		}
		statuses = append(statuses, status)
	}
	c.GetInvoice(context.Background(), "o1")
	if len(statuses) == 0 || statuses[0] != 503 {
		t.Errorf("observed %v, want the failed login as 503", statuses)
	}
}
