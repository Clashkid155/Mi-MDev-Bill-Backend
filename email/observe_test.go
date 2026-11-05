package email

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
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"OK"}`))
	}))
	defer srv.Close()
	c := New(settings(srv.URL))
	var statuses []int
	c.Observe = func(op string, status int, err error, took time.Duration) {
		if op == "" {
			t.Error("a call without a name")
		}
		statuses = append(statuses, status)
	}
	if err := c.Send(context.Background(), Message{To: "a@b.co", Subject: "Hi", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0] != 200 {
		t.Errorf("observed %v, want one 200", statuses)
	}
}
