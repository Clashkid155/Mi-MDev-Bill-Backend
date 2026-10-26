// Package ratelimit slows down callers that ask for too much, one bucket per
// client address. It lives in memory: a restart gives everyone a full bucket,
// which is fine for protecting a single server from floods and password guessing.
package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"Bills-Backend/reply"
)

// Limiter is a set of token buckets keyed by client address. A bucket holds up
// to `burst` tokens and refills at `perMinute`; every allowed request takes one.
type Limiter struct {
	perSecond float64
	burst     float64
	now       func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastClean time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New makes a limiter that lets a client make `burst` requests at once and
// `perMinute` a minute after that.
func New(perMinute, burst int) *Limiter {
	return &Limiter{
		perSecond: float64(perMinute) / 60,
		burst:     float64(burst),
		now:       time.Now,
		buckets:   map[string]*bucket{},
	}
}

// take brings the bucket up to date and returns it. The caller holds l.mu.
func (l *Limiter) take(key string) *bucket {
	now := l.now()
	l.clean(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
		return b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.perSecond)
	b.at = now
	return b
}

// clean forgets buckets that have refilled completely: they behave exactly like
// a client never seen, so a flood of one-off addresses cannot grow the map for long.
func (l *Limiter) clean(now time.Time) {
	if now.Sub(l.lastClean) < time.Minute {
		return
	}
	l.lastClean = now
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*l.perSecond >= l.burst {
			delete(l.buckets, key)
		}
	}
}

// Allow takes a token for key and says whether there was one.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.take(key)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Available says whether key has a token left, without taking it. Used to lock
// out an address after too many failed logins while still letting the first
// attempts through.
func (l *Limiter) Available(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.take(key).tokens >= 1
}

// RetryAfter is how many seconds until key has a token again (at least 1).
func (l *Limiter) RetryAfter(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	missing := 1 - l.take(key).tokens
	if missing <= 0 || l.perSecond <= 0 {
		return 1
	}
	return int(missing/l.perSecond) + 1
}

// Middleware answers 429 to a client that has used up its tokens.
func (l *Limiter) Middleware(trustProxy bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ClientIP(r, trustProxy)
		if !l.Allow(key) {
			TooMany(w, l.RetryAfter(key))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Shared answers 429 once everyone together has used up the tokens: one bucket
// for all callers. It protects something with a quota of its own (ABS allows our
// key only so many calls an hour) from being used up by a single caller.
func (l *Limiter) Shared(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow("*") {
			TooMany(w, l.RetryAfter("*"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TooMany writes the 429 answer.
func TooMany(w http.ResponseWriter, retryAfterSeconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	reply.Fail(w, http.StatusTooManyRequests, "too many requests, try again shortly")
}

// ClientIP is who is calling. Behind a reverse proxy (trustProxy) it is the last
// address the proxy put in X-Forwarded-For; otherwise the TCP peer. The header is
// only believed when asked for, because anyone can send one. IPv6 clients are
// grouped by their /64, since a single customer controls the whole block.
func ClientIP(r *http.Request, trustProxy bool) string {
	addr := r.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			addr = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// Lockout blocks an address outright for a long time after it gets something
// wrong too many times in a row, such as the admin token. Unlike a Limiter, which
// only slows a caller down, a locked address is refused until the time is up,
// even if its next attempt would have been right. A right attempt clears the count.
type Lockout struct {
	maxFailures int
	blockFor    time.Duration
	now         func() time.Time

	mu        sync.Mutex
	entries   map[string]*lockEntry
	lastClean time.Time
}

type lockEntry struct {
	failures     int
	lastFailure  time.Time
	blockedUntil time.Time
}

// NewLockout blocks an address for blockFor once it has failed maxFailures times in
// a row. Failures older than blockFor are forgotten, so an occasional typo
// never adds up.
func NewLockout(maxFailures int, blockFor time.Duration) *Lockout {
	return &Lockout{maxFailures: maxFailures, blockFor: blockFor, now: time.Now, entries: map[string]*lockEntry{}}
}

// Blocked says whether key is locked out and for how many more seconds.
func (l *Lockout) Blocked(key string) (retryAfterSeconds int, blocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return 0, false
	}
	if remaining := e.blockedUntil.Sub(l.now()); remaining > 0 {
		return int(remaining.Seconds()) + 1, true
	}
	return 0, false
}

// Fail records a wrong attempt by key and locks it out once there have been too many.
func (l *Lockout) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.clean(now)
	e := l.entries[key]
	if e == nil || now.Sub(e.lastFailure) > l.blockFor {
		e = &lockEntry{}
		l.entries[key] = e
	}
	e.failures++
	e.lastFailure = now
	if e.failures >= l.maxFailures {
		e.blockedUntil = now.Add(l.blockFor)
		e.failures = 0 // a fresh set of attempts once the block ends
	}
}

// Succeed clears key's failures: it got it right.
func (l *Lockout) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// clean forgets addresses that are no longer blocked and have not failed lately.
func (l *Lockout) clean(now time.Time) {
	if now.Sub(l.lastClean) < time.Minute {
		return
	}
	l.lastClean = now
	for key, e := range l.entries {
		if now.After(e.blockedUntil) && now.Sub(e.lastFailure) > l.blockFor {
			delete(l.entries, key)
		}
	}
}

// Locked writes the 429 answer for a locked-out address.
func Locked(w http.ResponseWriter, retryAfterSeconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	reply.Fail(w, http.StatusTooManyRequests, fmt.Sprintf("too many wrong tokens, this address is blocked for %d more minutes", (retryAfterSeconds+59)/60))
}
