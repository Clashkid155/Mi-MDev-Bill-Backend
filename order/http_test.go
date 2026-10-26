package order

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests below drive the real HTTP handlers, the way the app and the
// providers do, against fakes of Monnify, Kora and ABS.

func postJSON(t testing.TB, url string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case []byte:
		raw = b
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(body)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func getBody(t testing.TB, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

type orderReply struct {
	Data struct {
		ID      string `json:"id"`
		Status  Status `json:"status"`
		Payment *struct {
			AccountNo string `json:"account_number"`
		} `json:"payment"`
	} `json:"data"`
}

func TestAnOrderLifecycleOverHTTP(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/v1/orders", airtimeRequest)
	var created orderReply
	json.Unmarshal(body, &created)
	if code != 200 || created.Data.ID == "" || created.Data.Status != StatusAwaitingPayment || created.Data.Payment == nil {
		t.Fatalf("create answered %d: %s", code, body)
	}

	// The app retrying the same request (a dropped connection) gets the same order, not a second invoice.
	_, again := postJSON(t, srv.URL+"/v1/orders", airtimeRequest)
	var second orderReply
	json.Unmarshal(again, &second)
	if second.Data.ID != created.Data.ID || f.invoicesMade.Load() != 1 {
		t.Fatalf("retry made a new order: %s vs %s, invoices %d", second.Data.ID, created.Data.ID, f.invoicesMade.Load())
	}

	code, body = getBody(t, srv.URL+"/v1/orders/"+created.Data.ID)
	var fetched orderReply
	json.Unmarshal(body, &fetched)
	if code != 200 || fetched.Data.Status != StatusAwaitingPayment {
		t.Fatalf("status check answered %d: %s", code, body)
	}

	// What the app may see: never the customer's email or what the vendor charged us.
	lower := strings.ToLower(string(body))
	for _, secret := range []string{"a@b.co", "cost", "vend_response", "idempotency"} {
		if strings.Contains(lower, secret) {
			t.Errorf("the order view leaks %q: %s", secret, body)
		}
	}
}

func TestUnknownAndHostileOrderIdsAreJustNotFound(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()
	f.svc.Create(context.Background(), airtimeRequest)

	for _, id := range []string{"MDEV_doesnotexist", "' OR 1=1 --", "%27%20OR%20%271%27=%271", "..%2F..%2Fetc%2Fpasswd", strings.Repeat("A", 5000), "%00"} {
		code, body := getBody(t, srv.URL+"/v1/orders/"+id)
		if code != 404 && code != 400 {
			t.Errorf("id %.30q answered %d: %.100s", id, code, body)
		}
		if strings.Contains(string(body), "a@b.co") || strings.Contains(string(body), "MDEV_") && code == 200 {
			t.Errorf("id %.30q leaked an order: %s", id, body)
		}
	}
}

func TestBadRequestBodiesAreRefusedBeforeAnyProviderIsCalled(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()

	huge := `{"idempotency_key":"` + strings.Repeat("k", 2<<20) + `"}`
	bodies := map[string]string{
		"empty":            "",
		"not json":         "<xml/>",
		"json array":       "[1,2,3]",
		"wrong field type": `{"amount_kobo":"lots"}`,
		"too large":        huge,
		"nulls":            `{"idempotency_key":null,"email":null}`,
		"negative amount":  `{"idempotency_key":"n","kind":"airtime","network":"MTNAIR6TFD","recipient":"08012345678","amount_kobo":-500,"email":"a@b.co"}`,
		"overflow amount":  `{"idempotency_key":"o","kind":"airtime","network":"MTNAIR6TFD","recipient":"08012345678","amount_kobo":9223372036854775807,"email":"a@b.co"}`,
	}
	for name, body := range bodies {
		code, out := postJSON(t, srv.URL+"/v1/orders", body)
		if code < 400 || code >= 500 {
			t.Errorf("%s: answered %d, want a 4xx: %.120s", name, code, out)
		}
	}
	if f.invoicesMade.Load() != 0 || f.spendCalls.Load() != 0 {
		t.Fatalf("a refused request reached a provider: invoices %d, ABS spends %d", f.invoicesMade.Load(), f.spendCalls.Load())
	}
}

func TestMeterCheckOverHTTP(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/v1/meters/validate", `{"service_id":"ELEC1","meter":"12345678901"}`)
	if code != 200 || !strings.Contains(string(body), "JOHN DOE") {
		t.Fatalf("answered %d: %s", code, body)
	}
	if code, _ := postJSON(t, srv.URL+"/v1/meters/validate", "not json"); code != 400 {
		t.Errorf("bad body answered %d, want 400", code)
	}
	f.meterOK.Store(false)
	if code, _ := postJSON(t, srv.URL+"/v1/meters/validate", `{"service_id":"ELEC1","meter":"12345678901"}`); code == 200 || code >= 500 && code != 502 {
		t.Errorf("a meter ABS does not know answered %d", code)
	}
}

func absSignature(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + string(body)))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func postABS(t *testing.T, srv *httptest.Server, body []byte, ts, signature string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/webhooks/abs", bytes.NewReader(body))
	req.Header.Set("X-ABS-Timestamp", ts)
	req.Header.Set("X-ABS-Signature", signature)
	req.Header.Set("X-ABS-Delivery", "d-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestABSWebhookNeedsItsSignature(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc, ABSWebhookSecret: "abs-secret"}).Routes())
	defer srv.Close()
	body := []byte(`{"event":"transaction.settled","data":{"order_id":"1","status":"1"}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	if code := postABS(t, srv, body, now, absSignature("abs-secret", now, body)); code != 200 {
		t.Fatalf("a genuine webhook answered %d", code)
	}
	if code := postABS(t, srv, body, now, absSignature("wrong", now, body)); code != 401 {
		t.Errorf("a forged webhook answered %d, want 401", code)
	}
	old := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	if code := postABS(t, srv, body, old, absSignature("abs-secret", old, body)); code != 401 {
		t.Errorf("a replayed (old) webhook answered %d, want 401", code)
	}
	if code := postABS(t, srv, body, now, ""); code != 401 {
		t.Errorf("an unsigned webhook answered %d, want 401", code)
	}
	var stored int
	f.svc.Store.db.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE provider = 'abs'`).Scan(&stored)
	if stored != 1 {
		t.Errorf("only the genuine webhook should be stored, %d were", stored)
	}
}

func TestWithRealMoneyAnABSWebhookWithoutASecretIsRefused(t *testing.T) {
	f := newFakes(t)
	body := []byte(`{"event":"x"}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	live := httptest.NewServer((&API{Service: f.svc, RequireSignatures: true}).Routes())
	defer live.Close()
	if code := postABS(t, live, body, now, "sha256=00"); code != 401 {
		t.Errorf("with real money and no secret the webhook answered %d, want 401", code)
	}
	sandbox := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer sandbox.Close()
	if code := postABS(t, sandbox, body, now, ""); code != 200 {
		t.Errorf("in sandbox an unsigned webhook answered %d, want 200", code)
	}
}

// A payment whose webhook never arrived is found by the sweep, which is a loop on
// a timer; run it for real, quickly. Running again and again must not buy twice.
func TestTheSweepLoopSettlesAPaymentWhoseWebhookNeverCame(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	f.svc.Store.db.Exec(`UPDATE orders SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), o.ID)
	f.invoiceState.Store("PAID") // Monnify says it was paid
	f.invoiceKobo.Store(o.Due())

	api := &API{Service: f.svc}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { api.Sweep(runCtx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status == StatusSuccess {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let it go round a few more times
	stop()
	<-done
	api.Wait()

	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusSuccess {
		t.Errorf("a paid order is %s, want SUCCESS", got.Status)
	}
	if n := f.spendCalls.Load(); n != 1 {
		t.Errorf("ABS was called %d times for 1 order", n)
	}
}

func TestTheSweepLoopExpiresAnUnpaidOrderAndStopsWhenAsked(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	f.svc.Store.db.Exec(`UPDATE orders SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), o.ID)

	api := &API{Service: f.svc}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { api.Sweep(runCtx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status == StatusExpired {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not stop when its context ended")
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusExpired {
		t.Errorf("an unpaid order past its window is %s, want EXPIRED", got.Status)
	}
	if f.spendCalls.Load() != 0 {
		t.Error("nothing was paid, so nothing may be bought")
	}
}

// Kora retries webhooks and several can land at once. However many arrive, the
// customer is charged once and ABS is paid once.
func TestManyAtOnceWebhooksForOneOrderBuyOnlyOnce(t *testing.T) {
	f := newFakes(t)
	k := newKoraFake(t, f)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	api := &API{Service: f.svc, KoraSecret: koraSecret}
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()
	k.status.Store("success")
	k.acceptedKobo.Store(o.AmountKobo)

	body, sig := koraEvent("charge.success", o.ID)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", srv.URL+"/webhooks/kora", bytes.NewReader(body))
			req.Header.Set("x-korapay-signature", sig)
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != 200 {
				bad.Add(1)
			}
			if resp != nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	api.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d of 40 concurrent webhooks failed", bad.Load())
	}
	got, _ := f.svc.Store.ByID(ctx, o.ID)
	if got.Status != StatusSuccess || f.spendCalls.Load() != 1 {
		t.Fatalf("status %s, ABS called %d times; want SUCCESS and exactly 1", got.Status, f.spendCalls.Load())
	}
}

// A burst of customers at the same moment: every order is made, none is lost to
// "database is locked", and a retried request is still one order.
func TestManyCustomersAtOnce(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()

	const customers = 60
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < customers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := airtimeRequest
			req.IdempotencyKey = fmt.Sprintf("customer-%d", i%30) // every key is used twice, at the same time
			code, _ := postJSON(t, srv.URL+"/v1/orders", req)
			if code != 200 {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Errorf("%d of %d requests failed", failures.Load(), customers)
	}
	var orders int
	f.svc.Store.db.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&orders)
	if orders != 30 {
		t.Errorf("%d orders for 30 distinct keys; a duplicated request must not make a second order", orders)
	}
	if n := f.invoicesMade.Load(); n != 30 {
		t.Errorf("%d payment accounts asked for 30 orders; a duplicate request must reuse the first one", n)
	}
}

// FuzzCreateOrder throws random bodies at the order endpoint. Whatever arrives,
// the server must answer 200 or a client error, never crash and never answer 500.
// `go test` runs the seeds; run `go test -fuzz=FuzzCreateOrder ./order` to explore.
func FuzzCreateOrder(f *testing.F) {
	fk := newFakes(f)
	handler := (&API{Service: fk.svc}).Routes()
	seed, _ := json.Marshal(airtimeRequest)
	f.Add(seed)
	f.Add([]byte(`{"idempotency_key":"x","kind":"data","plan_id":"P1","recipient":"08012345678","email":"a@b.co"}`))
	f.Add([]byte(`{"idempotency_key":"e","kind":"electricity","service_id":"ELEC1","recipient":"12345678901","amount_kobo":100000,"email":"a@b.co"}`))
	f.Add([]byte(`{"amount_kobo":-1}`))
	f.Add([]byte(`{"email":"\u0000‮@x"}`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequest("POST", "/v1/orders", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == 500 {
			t.Fatalf("500 for body %q: %s", body, rec.Body.String())
		}
	})
}
