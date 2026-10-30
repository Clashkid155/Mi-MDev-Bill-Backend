package main

import (
	"bytes"
	"encoding/json"
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
	"Bills-Backend/notify"
	"Bills-Backend/order"
	"Bills-Backend/platform"
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
	shared, err := platform.NewStore(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	admin := &platform.Admin{Store: shared, Log: log, Alerts: alerts}
	noticeStore, err := notify.NewStore(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	notices := &notify.Service{Store: noticeStore, Log: log}
	api.Gate = notices
	return routes(cfg, log, api, admin, notices, direct), &logs
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

// bearer makes a request with the given token and returns the status and the body.
func bearer(h http.Handler, method, path, token, remote, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestNamedTokensWorkThroughTheRealRoutes(t *testing.T) {
	h, logs := testServer(t, Config{AdminToken: "owner-secret"}, nil)
	code, body := bearer(h, "POST", "/admin/tokens", "owner-secret", "192.0.2.1:1", `{"name":"dashboard","role":"viewer"}`)
	var made struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &made); err != nil || code != 200 || made.Data.Token == "" {
		t.Fatalf("making a token: %d %s", code, body)
	}
	viewer := made.Data.Token

	if code, _ := bearer(h, "GET", "/admin/orders", viewer, "192.0.2.2:1", ""); code != 200 {
		t.Errorf("a viewer reading orders: %d", code)
	}
	if code, _ := bearer(h, "POST", "/admin/orders/MDEV_9616f67242edcfc1f3a20872b77b29ba/refunded", viewer, "192.0.2.2:1", `{}`); code != 403 {
		t.Errorf("a viewer marking a refund: %d, want 403", code)
	}
	if code, _ := bearer(h, "GET", "/admin/tokens", viewer, "192.0.2.2:1", ""); code != 403 {
		t.Errorf("a viewer listing tokens: %d, want 403", code)
	}

	// The owner's changes are still named by route in the access log, never by order id.
	bearer(h, "POST", "/admin/orders/MDEV_9616f67242edcfc1f3a20872b77b29ba/refunded", "owner-secret", "192.0.2.3:1", `{}`)
	if strings.Contains(logs.String(), "9616f67242edcfc1f3a20872b77b29ba") {
		t.Errorf("an order id reached the access log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "POST /admin/orders/{id}/refunded") {
		t.Errorf("the access log should still name the route:\n%s", logs.String())
	}

	// And the token is gone the moment it is revoked.
	if code, _ := bearer(h, "DELETE", "/admin/tokens/1", "owner-secret", "192.0.2.3:1", ""); code != 200 {
		t.Fatalf("revoking: %d", code)
	}
	if code, _ := bearer(h, "GET", "/admin/orders", viewer, "192.0.2.4:1", ""); code != 401 {
		t.Errorf("a revoked token: %d, want 401", code)
	}
}

func TestALockoutIsInTheAuditLog(t *testing.T) {
	h, _ := testServer(t, Config{AdminToken: "owner-secret"}, nil)
	attacker := "198.51.100.77:1"
	for i := 0; i < adminMaxWrongTokens; i++ {
		bearer(h, "GET", "/admin/orders", fmt.Sprintf("wrong%d", i), attacker, "")
	}
	_, body := bearer(h, "GET", "/admin/audit", "owner-secret", "192.0.2.9:1", "")
	if !strings.Contains(body, "admin lockout") || !strings.Contains(body, "198.51.100.77") || strings.Contains(body, "wrong0") {
		t.Errorf("the lockout should be in the audit log with the address and no guessed token: %s", body)
	}
}

// The whole path an owner uses before maintenance: publish a notice that stops orders from the admin
// app, see the customer app told about it and refused new orders, then end it and see orders resume.
func TestAMaintenanceNoticeStopsNewOrdersUntilItIsEnded(t *testing.T) {
	h, logs := testServer(t, Config{AdminToken: "s3cret"}, nil)

	send := func(method, path, auth, body string, headers ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "192.0.2.50:1000"
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
	order := `{"idempotency_key":"maintenance-test-0001","kind":"airtime","network":"MTNAIR6TFD","recipient":"08012345678","amount_kobo":10000,"email":"a@b.co"}`
	customer := []string{"X-App", "mi-mdev", "X-Platform", "android", "X-App-Version", "1.4.2"}

	if rec := send("POST", "/v1/orders", "", order, customer...); rec.Code == 503 {
		t.Fatalf("orders were refused with no maintenance notice: %s", rec.Body)
	}

	made := send("POST", "/admin/notifications", "Bearer s3cret",
		`{"kind":"maintenance","title":"Back at 3am","body":"Upgrading.","blocking":true,"apps":["mi-mdev"]}`)
	if made.Code != 200 {
		t.Fatalf("publishing answered %d: %s", made.Code, made.Body)
	}
	var notice struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(made.Body.Bytes(), &notice)

	// The app is told (no login needed) ...
	seen := send("GET", "/v1/notifications", "", "", customer...)
	if seen.Code != 200 || !strings.Contains(seen.Body.String(), `"blocking_now":true`) {
		t.Errorf("the app was not told: %d %s", seen.Code, seen.Body)
	}
	// ... and new orders from it are refused, in a shape the app can act on.
	refused := send("POST", "/v1/orders", "", order, customer...)
	if refused.Code != 503 || !strings.Contains(refused.Body.String(), `"code":"maintenance"`) || !strings.Contains(refused.Body.String(), "Upgrading.") {
		t.Errorf("a new order during maintenance: %d %s", refused.Code, refused.Body)
	}
	// A refusal the owner chose is a warning in the log (so it never fills the events with "errors"),
	// and the marker that says so never reaches the app.
	if refused.Header().Get("X-Planned-Refusal") != "" {
		t.Error("the logging marker leaked to the client")
	}
	if got := logs.String(); !strings.Contains(got, "[WARN]   [HTTP]  POST /v1/orders -> 503") || strings.Contains(got, "[ERROR]  [HTTP]  POST /v1/orders -> 503") {
		t.Errorf("a planned refusal must log as a warning:\n%s", got)
	}
	// The dashboard is a different app: the notice names only the customer app.
	if rec := send("POST", "/v1/orders", "", order, "X-App", "dashboard"); rec.Code == 503 {
		t.Error("a notice for the customer app must not stop other apps")
	}
	// Checking an order or reading notices is never blocked, and neither is the admin app.
	if rec := send("GET", "/admin/notifications", "Bearer s3cret", ""); rec.Code != 200 {
		t.Errorf("the admin app answered %d during maintenance", rec.Code)
	}

	// Ending it lifts the block at once.
	if rec := send("DELETE", fmt.Sprintf("/admin/notifications/%d", notice.Data.ID), "Bearer s3cret", ""); rec.Code != 200 {
		t.Fatalf("ending answered %d: %s", rec.Code, rec.Body)
	}
	if rec := send("POST", "/v1/orders", "", order, customer...); rec.Code == 503 {
		t.Errorf("orders were still refused after the notice ended: %s", rec.Body)
	}
	if seen := send("GET", "/v1/notifications", "", "", customer...); strings.Contains(seen.Body.String(), "Back at 3am") {
		t.Errorf("the ended notice is still shown: %s", seen.Body)
	}
}
