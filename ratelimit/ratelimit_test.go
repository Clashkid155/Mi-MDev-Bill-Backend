package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// clock lets a test move time by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func limiterWithClock(perMinute, burst int) (*Limiter, *clock) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := New(perMinute, burst)
	l.now = c.now
	return l, c
}

func TestBurstThenRefill(t *testing.T) {
	l, c := limiterWithClock(60, 3) // one token a second, 3 at once
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	if l.Allow("a") {
		t.Fatal("the fourth request should be refused")
	}
	c.t = c.t.Add(1500 * time.Millisecond)
	if !l.Allow("a") {
		t.Fatal("a token should have come back after 1.5s")
	}
	if l.Allow("a") {
		t.Fatal("only one token should have come back")
	}
}

func TestClientsAreIndependent(t *testing.T) {
	l, _ := limiterWithClock(60, 1)
	if !l.Allow("a") || l.Allow("a") {
		t.Fatal("a should get exactly one")
	}
	if !l.Allow("b") {
		t.Fatal("b must not be affected by a")
	}
}

func TestAvailableDoesNotSpend(t *testing.T) {
	l, _ := limiterWithClock(60, 1)
	for i := 0; i < 5; i++ {
		if !l.Available("a") {
			t.Fatal("peeking must not use the token")
		}
	}
	l.Allow("a")
	if l.Available("a") {
		t.Fatal("no token should be left")
	}
}

func TestIdleBucketsAreForgotten(t *testing.T) {
	l, c := limiterWithClock(60, 5)
	for i := 0; i < 100; i++ {
		l.Allow(string(rune('a' + i%26)))
	}
	c.t = c.t.Add(10 * time.Minute)
	l.Allow("fresh")
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > 2 {
		t.Fatalf("%d buckets kept after everyone went quiet", n)
	}
}

func TestMiddlewareAnswers429WithRetryAfter(t *testing.T) {
	l, _ := limiterWithClock(60, 2)
	h := l.Middleware(false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	var codes []int
	var retry string
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "198.51.100.7:4000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
		retry = rec.Header().Get("Retry-After")
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("codes %v, want 200 200 429", codes)
	}
	if retry == "" {
		t.Error("a refused request must say when to come back")
	}
}

func TestClientIP(t *testing.T) {
	req := func(remote, forwarded string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		return r
	}
	cases := []struct {
		name  string
		r     *http.Request
		trust bool
		want  string
	}{
		{"plain peer", req("203.0.113.9:5555", ""), false, "203.0.113.9"},
		{"header ignored when not trusted", req("203.0.113.9:5555", "1.2.3.4"), false, "203.0.113.9"},
		{"proxy header believed when trusted", req("127.0.0.1:1", "1.2.3.4"), true, "1.2.3.4"},
		{"a spoofed first entry does not count", req("127.0.0.1:1", "6.6.6.6, 1.2.3.4"), true, "1.2.3.4"},
		{"trusted but no header", req("203.0.113.9:5555", ""), true, "203.0.113.9"},
		{"ipv6 grouped by /64", req("[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:80", ""), false, "2001:db8:1:2::"},
	}
	for _, c := range cases {
		if got := ClientIP(c.r, c.trust); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLockoutBlocksAfterTooManyFailuresThenReleases(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := NewLockout(3, 15*time.Minute)
	l.now = c.now

	for i := 0; i < 2; i++ {
		l.Fail("a")
	}
	if _, blocked := l.Blocked("a"); blocked {
		t.Fatal("two failures of three must not block")
	}
	l.Fail("a")
	wait, blocked := l.Blocked("a")
	if !blocked || wait < 14*60 {
		t.Fatalf("three failures must block for about 15 minutes: blocked %t, wait %ds", blocked, wait)
	}
	if _, blocked := l.Blocked("b"); blocked {
		t.Error("another address must not be blocked")
	}

	c.t = c.t.Add(14 * time.Minute)
	if _, blocked := l.Blocked("a"); !blocked {
		t.Error("still blocked after 14 minutes")
	}
	c.t = c.t.Add(2 * time.Minute)
	if _, blocked := l.Blocked("a"); blocked {
		t.Error("should be released after 15 minutes")
	}
	// A fresh set of attempts after release.
	l.Fail("a")
	l.Fail("a")
	if _, blocked := l.Blocked("a"); blocked {
		t.Error("the count starts again after a release")
	}
}

func TestLockoutForgetsOldTyposAndClearsOnSuccess(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := NewLockout(3, 15*time.Minute)
	l.now = c.now

	l.Fail("a")
	l.Fail("a")
	c.t = c.t.Add(20 * time.Minute) // the two typos are old news
	l.Fail("a")
	if _, blocked := l.Blocked("a"); blocked {
		t.Error("typos spread over hours must not add up to a lockout")
	}
	l.Fail("a")
	l.Succeed("a") // got it right: the count goes back to zero
	l.Fail("a")
	l.Fail("a")
	if _, blocked := l.Blocked("a"); blocked {
		t.Error("a success must clear the failures")
	}
}
