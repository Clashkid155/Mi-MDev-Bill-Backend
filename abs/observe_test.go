package abs

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
		http.Error(w, "oops", http.StatusBadGateway)
	}))
	c := New(srv.URL, "k", true)
	var ops []string
	var statuses []int
	c.Observe = func(op string, status int, err error, took time.Duration) {
		ops, statuses = append(ops, op), append(statuses, status)
	}
	c.Balance(context.Background())
	srv.Close()
	c.Balance(context.Background()) // nobody answers any more
	if len(ops) != 2 || ops[0] == "" || statuses[0] != 502 || statuses[1] != 0 {
		t.Errorf("observed %v %v, want a 502 then no answer", ops, statuses)
	}
}
