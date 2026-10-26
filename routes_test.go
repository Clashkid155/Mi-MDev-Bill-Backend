package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/logging"
	"Bills-Backend/order"
)

// testServer is the real route table over a real (temporary) database. Nothing
// in these tests reaches a payment provider or ABS.
func testServer(t *testing.T, cfg Config, direct http.Handler) (http.Handler, *bytes.Buffer) {
	t.Helper()
	return testServerWith(t, cfg, direct, nil)
}

// testServerWith is testServer with the owner's alerts going to alerts.
func testServerWith(t *testing.T, cfg Config, direct http.Handler, alerts order.Alerter) (http.Handler, *bytes.Buffer) {
	t.Helper()
	store, err := order.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var logs bytes.Buffer
	log := logging.New(&logs, slog.LevelInfo)
	vendor := abs.New("http://127.0.0.1:1", "k", true) // nothing listens there: a meter check that gets through fails fast with 502
	api := &order.API{Service: &order.Service{Store: store, Vendor: vendor, Log: log, Alerts: alerts}, KoraSecret: "sk_test"}
	return routes(cfg, log, api, direct), &logs
}

func do(h http.Handler, method, path, auth, remote string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if remote != "" {
		req.RemoteAddr = remote
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminRoutesNeedTheToken(t *testing.T) {
	h, _ := testServer(t, Config{AdminToken: "s3cret"}, nil)
	n := 0
	fresh := func() string { n++; return fmt.Sprintf("192.0.2.%d:1000", n) } // a new address each time, so no lockout
	for _, path := range []string{"/admin/orders", "/admin/stats", "/admin/balances", "/admin/settings", "/admin/expenses", "/admin/webhooks"} {
		if code := do(h, "GET", path, "", fresh()).Code; code != 401 {
			t.Errorf("GET %s with no token answered %d, want 401", path, code)
		}
		if code := do(h, "GET", path, "Bearer nope", fresh()).Code; code != 401 {
			t.Errorf("GET %s with a wrong token answered %d, want 401", path, code)
		}
	}
	// A refund can be recorded or an expense deleted only by the admin too.
	for method, path := range map[string]string{"POST": "/admin/orders/MDEV_x/refunded", "DELETE": "/admin/expenses/1", "PUT": "/admin/settings/markup.mtn"} {
		if code := do(h, method, path, "", fresh()).Code; code != 401 {
			t.Errorf("%s %s with no token answered %d, want 401", method, path, code)
		}
	}
	if code := do(h, "GET", "/admin/orders", "Bearer s3cret", fresh()).Code; code != 200 {
		t.Errorf("the right token answered %d, want 200", code)
	}
}

func TestGuessingTheAdminTokenLocksTheAddressOutForGood(t *testing.T) {
	h, _ := testServer(t, Config{AdminToken: "s3cret"}, nil)
	attacker := "198.51.100.9:5000"
	for i := 0; i < adminMaxWrongTokens; i++ {
		if code := do(h, "GET", "/admin/orders", fmt.Sprintf("Bearer guess%d", i), attacker).Code; code != 401 {
			t.Fatalf("guess %d answered %d, want 401", i, code)
		}
	}
	// Now it is blocked: even the right token is refused, and for a long time.
	rec := do(h, "GET", "/admin/orders", "Bearer s3cret", attacker)
	retry, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != 429 || retry < int((adminLockFor-time.Minute).Seconds()) {
		t.Errorf("after too many wrong guesses the address must be blocked for about %v: got %d, retry after %ds", adminLockFor, rec.Code, retry)
	}
	if code := do(h, "GET", "/admin/orders", "Bearer wrong", attacker).Code; code != 429 {
		t.Errorf("a blocked address kept guessing and got %d, want 429", code)
	}
	if code := do(h, "GET", "/admin/orders", "Bearer s3cret", "203.0.113.5:1").Code; code != 200 {
		t.Errorf("someone else must not be locked out by the attacker: %d", code)
	}
}

func TestAnAdminWhoMistypesIsNotLockedOut(t *testing.T) {
	h, _ := testServer(t, Config{AdminToken: "s3cret"}, nil)
	me := "192.0.2.77:1"
	for round := 0; round < 3; round++ { // a few typos, then the right token, again and again
		for i := 0; i < adminMaxWrongTokens-1; i++ {
			do(h, "GET", "/admin/orders", "Bearer typo", me)
		}
		if code := do(h, "GET", "/admin/orders", "Bearer s3cret", me).Code; code != 200 {
			t.Fatalf("round %d: the right token answered %d, want 200", round, code)
		}
	}
}

func TestDirectProviderRoutes(t *testing.T) {
	direct := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	live, _ := testServer(t, Config{AdminToken: "s3cret"}, nil) // real money: not mounted at all
	for _, prefix := range []string{"/abs/purchase", "/kora/charge", "/monnify/x", "/email/send"} {
		if code := do(live, "POST", prefix, "Bearer s3cret", "").Code; code != 404 {
			t.Errorf("%s answered %d with real money in use, want 404 even with the token", prefix, code)
		}
	}
	sandbox, _ := testServer(t, Config{AdminToken: "s3cret"}, direct) // sandbox: mounted, still behind the token
	if code := do(sandbox, "POST", "/abs/purchase", "", "").Code; code != 401 {
		t.Errorf("a sandbox direct route with no token answered %d, want 401", code)
	}
	if code := do(sandbox, "POST", "/abs/purchase", "Bearer s3cret", "").Code; code != 200 {
		t.Errorf("a sandbox direct route with the token answered %d, want 200", code)
	}
}

func TestEveryAnswerCarriesTheSafetyHeaders(t *testing.T) {
	h, _ := testServer(t, Config{AdminToken: "s3cret"}, nil)
	for _, path := range []string{"/healthz", "/v1/orders/MDEV_x", "/admin/orders", "/no/such/page"} {
		rec := do(h, "GET", path, "", "")
		for header, want := range map[string]string{
			"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s has no Content-Security-Policy", path)
		}
	}
	if rec := do(h, "GET", "/healthz", "", ""); rec.Code != 200 {
		t.Errorf("health check answered %d", rec.Code)
	}
}

func TestOneAddressCannotFloodTheOrderEndpoints(t *testing.T) {
	h, _ := testServer(t, Config{}, nil)
	flooder := "198.51.100.20:1000"

	var limited int
	for i := 0; i < readsBurst+20; i++ {
		if do(h, "GET", "/v1/orders/MDEV_x", "", flooder).Code == 429 {
			limited++
		}
	}
	if limited < 15 {
		t.Errorf("only %d of %d status checks were limited", limited, readsBurst+20)
	}
	limited = 0
	for i := 0; i < writesBurst+10; i++ { // an unreadable body is refused (400) and still counts
		if do(h, "POST", "/v1/orders", "", flooder).Code == 429 {
			limited++
		}
	}
	if limited < 8 {
		t.Errorf("only %d of %d new orders were limited", limited, writesBurst+10)
	}
	// Another customer is untouched, and so are the health check and the providers' webhooks.
	if code := do(h, "GET", "/v1/orders/MDEV_x", "", "203.0.113.77:1").Code; code == 429 {
		t.Error("an innocent address was limited")
	}
	if code := do(h, "GET", "/healthz", "", flooder).Code; code != 200 {
		t.Errorf("the health check was limited: %d", code)
	}
	for i := 0; i < 300; i++ {
		if do(h, "POST", "/webhooks/kora", "", flooder).Code == 429 {
			t.Fatal("provider webhooks must never be rate limited")
		}
	}
}

func TestForwardedForIsOnlyBelievedBehindATrustedProxy(t *testing.T) {
	// Not behind a proxy: a client cannot dodge the limit by inventing addresses.
	direct, _ := testServer(t, Config{}, nil)
	var limited int
	for i := 0; i < readsBurst+30; i++ {
		if do(direct, "GET", "/v1/orders/x", "", "198.51.100.30:1", "X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i%250)).Code == 429 {
			limited++
		}
	}
	if limited == 0 {
		t.Error("a forged X-Forwarded-For let a client escape the limit")
	}

	// Behind Caddy: every customer arrives from 127.0.0.1 and is told apart by the header.
	proxied, _ := testServer(t, Config{TrustProxy: true}, nil)
	for i := 0; i < readsBurst+30; i++ {
		who := fmt.Sprintf("203.0.113.%d", i%250)
		if code := do(proxied, "GET", "/v1/orders/x", "", "127.0.0.1:1", "X-Forwarded-For", who).Code; code == 429 {
			t.Fatalf("customer %d was limited because of other customers behind the same proxy", i)
		}
	}
}

func TestTheAccessLogNeverWritesAnOrderId(t *testing.T) {
	h, logs := testServer(t, Config{AdminToken: "s3cret"}, nil)
	do(h, "GET", "/v1/orders/MDEV_9616f67242edcfc1f3a20872b77b29ba", "", "")
	do(h, "POST", "/admin/orders/MDEV_9616f67242edcfc1f3a20872b77b29ba/refunded", "Bearer s3cret", "")
	out := logs.String()
	if strings.Contains(out, "9616f67242edcfc1f3a20872b77b29ba") {
		t.Errorf("an order id reached the access log:\n%s", out)
	}
	if !strings.Contains(out, "GET /v1/orders/{id}") || !strings.Contains(out, "POST /admin/orders/{id}/refunded") {
		t.Errorf("the log should name the route:\n%s", out)
	}
}

// Every meter check is a call to ABS, which allows our key only so many an hour.
func TestMeterChecksCannotUseUpTheABSQuota(t *testing.T) {
	body := func(h http.Handler, remote string) int {
		return do(h, "POST", "/v1/meters/validate", "", remote).Code
	}

	// One address gets a few, then must wait.
	h, _ := testServer(t, Config{}, nil)
	var limited int
	for i := 0; i < metersBurst+5; i++ {
		if body(h, "198.51.100.40:1") == 429 {
			limited++
		}
	}
	if limited < 4 {
		t.Errorf("one address made %d meter checks and only %d were limited", metersBurst+5, limited)
	}

	// Many addresses together are held to the shared total, so an attacker with
	// many addresses still cannot take the whole hourly quota.
	h, _ = testServer(t, Config{}, nil)
	passed := 0
	for i := 0; i < 100; i++ {
		if body(h, fmt.Sprintf("203.0.113.%d:1", i)) != 429 {
			passed++
		}
	}
	if passed > metersTotalBurst+2 {
		t.Errorf("%d meter checks from 100 different addresses got through; the shared total is %d", passed, metersTotalBurst)
	}
	if passed < metersTotalBurst {
		t.Errorf("only %d got through, the shared allowance of %d was not honoured", passed, metersTotalBurst)
	}
}

// recorder is an Alerter that remembers what it was told.
type recorder struct {
	mu   sync.Mutex
	sent []string
}

func (r *recorder) Alert(key string, throttle time.Duration, subject, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, subject+" | "+body)
}

func TestTheOwnerIsToldWhenSomeoneGuessesTheAdminToken(t *testing.T) {
	alerts := &recorder{}
	h, _ := testServerWith(t, Config{AdminToken: "s3cret"}, nil, alerts)
	for i := 0; i < adminMaxWrongTokens+3; i++ { // more guesses after the lockout must not repeat the alert
		do(h, "GET", "/admin/orders", fmt.Sprintf("Bearer guess%d", i), "198.51.100.9:5000")
	}
	alerts.mu.Lock()
	defer alerts.mu.Unlock()
	if len(alerts.sent) != 1 {
		t.Fatalf("want one alert, got %d: %q", len(alerts.sent), alerts.sent)
	}
	if !strings.Contains(alerts.sent[0], "guessing the admin token") || !strings.Contains(alerts.sent[0], "198.51.100.9") ||
		strings.Contains(alerts.sent[0], "guess0") || strings.Contains(alerts.sent[0], "s3cret") {
		t.Errorf("the alert should name the address and never include a token: %q", alerts.sent[0])
	}
}
