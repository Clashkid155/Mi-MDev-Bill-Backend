package kora

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// What the server's provider health is built from: every request reports how it went.
func TestEveryRequestIsReportedToTheObserver(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	c := New("http://127.0.0.1:1", "sk")
	c.Observe = func(op string, status int, err error, took time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, op)
		if took < 0 {
			t.Error("a negative duration")
		}
		if status == 0 && err == nil {
			t.Error("no answer must come with an error")
		}
	}

	// Nothing listens there: no answer.
	if _, err := c.Balance(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	// Kora answers 500, with no logger set (the observer must not depend on one).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"status":false,"message":"Internal server error"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	c.baseURL = srv.URL
	if _, err := c.Balance(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if len(seen) != 2 || seen[0] == "" {
		t.Errorf("observed %v, want 2 named calls", seen)
	}
}
