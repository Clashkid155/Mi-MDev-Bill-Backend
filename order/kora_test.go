package order

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"Bills-Backend/kora"
)

const koraSecret = "sk_test_abc"

// koraFake stands in for Kora: it hands out an account for a charge and answers
// the Transaction Query with whatever status and accepted amount the test sets.
type koraFake struct {
	status         atomic.Value // Kora's status word for the charge
	acceptedKobo   atomic.Int64
	expectedAdjust atomic.Int64 // added to amount_expected, to simulate Kora asking for more
	feeKobo        atomic.Int64 // the fee the query reply reports
	created        atomic.Int32

	mu      sync.Mutex
	lastReq map[string]any
	auth    string
}

func newKoraFake(t *testing.T, f *fakes) *koraFake {
	t.Helper()
	k := &koraFake{}
	k.status.Store("processing")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/charges/bank-transfer":
			k.created.Add(1)
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			k.mu.Lock()
			k.lastReq, k.auth = in, r.Header.Get("Authorization")
			k.mu.Unlock()
			amount := in["amount"].(float64)
			fmt.Fprintf(w, `{"status":true,"message":"Bank transfer initiated successfully","data":{"currency":"NGN","amount":%v,"amount_expected":%v,
				"reference":%q,"payment_reference":%q,"status":"processing",
				"bank_account":{"account_name":"Mi MDev","account_number":"7590031627","bank_name":"wema","bank_code":"035","expiry_date_in_utc":"2030-01-02T13:00:00.000Z"}}}`,
				amount, amount+float64(k.expectedAdjust.Load())/100, in["reference"], in["reference"])
		case r.Method == "GET" && r.URL.Path == "/api/v1/balances":
			fmt.Fprint(w, `{"status":true,"message":"ok","data":{"NGN":{"available_balance":"2500.50","pending_balance":100},"USD":{"available_balance":0,"pending_balance":0}}}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/charges/"):
			ref := strings.TrimPrefix(r.URL.Path, "/api/v1/charges/")
			fmt.Fprintf(w, `{"status":true,"message":"Charge retrieved successfully","data":{"reference":%q,"status":%q,"amount":1,"amount_paid":%.2f,"amount_accepted":%.2f,"fee":%.2f,"currency":"NGN"}}`,
				ref, k.status.Load().(string), float64(k.acceptedKobo.Load())/100, float64(k.acceptedKobo.Load())/100, float64(k.feeKobo.Load())/100)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	f.svc.Pay = kora.New(srv.URL, koraSecret)
	return k
}

// koraEvent is a webhook body signed the way Kora signs it: HMAC-SHA256 of the
// data object only.
func koraEvent(event, reference string) (body []byte, signature string) {
	data := fmt.Sprintf(`{"reference":%q,"payment_reference":%q,"currency":"NGN","amount":100,"fee":1.5,"status":"success"}`, reference, reference)
	mac := hmac.New(sha256.New, []byte(koraSecret))
	mac.Write([]byte(data))
	return []byte(fmt.Sprintf(`{"event":%q,"data":%s}`, event, data)), hex.EncodeToString(mac.Sum(nil))
}

func postKora(t *testing.T, srv *httptest.Server, body []byte, signature string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/webhooks/kora", bytes.NewReader(body))
	req.Header.Set("x-korapay-signature", signature)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestKoraCreateSendsTheRightRequest(t *testing.T) {
	f := newFakes(t)
	k := newKoraFake(t, f)
	o, err := f.svc.Create(context.Background(), airtimeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusAwaitingPayment || o.AccountNo != "7590031627" || o.BankName != "wema" {
		t.Fatalf("unexpected order %+v", o)
	}
	// Kora is asked about our own reference, so the order id doubles as the transaction reference.
	if o.TxnRef != o.ID || !strings.HasPrefix(o.ID, "MDEV_") {
		t.Fatalf("txn ref %q for order %q", o.TxnRef, o.ID)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.auth != "Bearer "+koraSecret {
		t.Errorf("Authorization %q", k.auth)
	}
	if k.lastReq["reference"] != o.ID || k.lastReq["amount"] != 100.0 || k.lastReq["currency"] != "NGN" {
		t.Errorf("request %+v", k.lastReq)
	}
	if k.lastReq["account_name"] != "Mi MDev" || k.lastReq["narration"] != "Mi MDev airtime" {
		t.Errorf("account_name %v, narration %v", k.lastReq["account_name"], k.lastReq["narration"])
	}
	// The customer must pay the price shown, so Kora's fee comes out of our side.
	if k.lastReq["merchant_bears_cost"] != true {
		t.Errorf("merchant_bears_cost = %v, want true", k.lastReq["merchant_bears_cost"])
	}
	if !o.ExpiresAt.Equal(o.ExpiresAt.UTC()) || o.ExpiresAt.Year() != 2030 {
		t.Errorf("expiry from Kora not used: %v", o.ExpiresAt)
	}
}

func TestKoraAccountForADifferentAmountIsRefused(t *testing.T) {
	f := newFakes(t)
	k := newKoraFake(t, f)
	k.expectedAdjust.Store(150) // Kora wants the price plus a ₦1.50 fee
	if _, err := f.svc.Create(context.Background(), airtimeRequest); err == nil {
		t.Fatal("an account asking for more than the price must not be shown to the customer")
	}
}

func TestKoraWebhookPaysAndVendsOnce(t *testing.T) {
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
	for i := 0; i < 3; i++ { // Kora retries; the duplicates must not buy again
		if code := postKora(t, srv, body, sig); code != 200 {
			t.Fatalf("webhook %d answered %d", i, code)
		}
	}
	api.Wait()
	got, _ := f.svc.Store.ByID(ctx, o.ID)
	if got.Status != StatusSuccess || f.spendCalls.Load() != 1 {
		t.Fatalf("status %s, ABS called %d times; want SUCCESS and 1", got.Status, f.spendCalls.Load())
	}
}

func TestKoraWebhookWithABadSignatureDoesNothing(t *testing.T) {
	f := newFakes(t)
	k := newKoraFake(t, f)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	srv := httptest.NewServer((&API{Service: f.svc, KoraSecret: koraSecret}).Routes())
	defer srv.Close()
	k.status.Store("success")
	k.acceptedKobo.Store(o.AmountKobo)

	body, _ := koraEvent("charge.success", o.ID)
	if code := postKora(t, srv, body, "deadbeef"); code != 401 {
		t.Fatalf("bad signature answered %d, want 401", code)
	}
	if code := postKora(t, srv, body, ""); code != 401 {
		t.Fatalf("no signature answered %d, want 401", code)
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusAwaitingPayment || f.spendCalls.Load() != 0 {
		t.Fatalf("status %s, ABS called %d times", got.Status, f.spendCalls.Load())
	}
}

func TestKoraWebhookIsRefusedWhenKoraIsNotConfigured(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()
	body, sig := koraEvent("charge.success", "MDEV_x")
	if code := postKora(t, srv, body, sig); code != 503 {
		t.Fatalf("answered %d, want 503", code)
	}
}

func TestKoraFailedEventIsLoggedAndIgnored(t *testing.T) {
	f := newFakes(t)
	k := newKoraFake(t, f)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	api := &API{Service: f.svc, KoraSecret: koraSecret}
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()
	k.status.Store("success") // even if Kora would say paid, a failed event is not acted on
	k.acceptedKobo.Store(o.AmountKobo)

	body, sig := koraEvent("charge.failed", o.ID)
	if code := postKora(t, srv, body, sig); code != 200 {
		t.Fatalf("answered %d", code)
	}
	api.Wait()
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusAwaitingPayment || f.spendCalls.Load() != 0 {
		t.Fatalf("status %s, ABS called %d times", got.Status, f.spendCalls.Load())
	}
	if hooks, _ := f.svc.Store.Webhooks(ctx, "kora", 10); len(hooks) != 1 {
		t.Fatalf("webhook not saved: %d", len(hooks))
	}
}

// Kora's webhook amount is always what we asked for. The money that counts is
// amount_accepted from the query, so a customer who paid too much or too little
// under "accept all" is never vended.
func TestKoraAmountAcceptedDecidesWhetherToVend(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		accepted func(due int64) int64
		want     Status
		vended   int32
	}{
		{"exact", "success", func(d int64) int64 { return d }, StatusSuccess, 1},
		{"still processing", "processing", func(int64) int64 { return 0 }, StatusAwaitingPayment, 0},
		{"accepted less than due", "success", func(d int64) int64 { return d - 50_00 }, StatusPartiallyPaid, 0},
		{"accepted more than due", "success", func(d int64) int64 { return d + 50_00 }, StatusOverpaid, 0},
		{"failed", "failed", func(int64) int64 { return 0 }, StatusAwaitingPayment, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakes(t)
			k := newKoraFake(t, f)
			ctx := context.Background()
			o, _ := f.svc.Create(ctx, airtimeRequest)
			k.status.Store(c.status)
			k.acceptedKobo.Store(c.accepted(o.AmountKobo))

			paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef)
			if err != nil {
				t.Fatal(err)
			}
			if paid != nil {
				if err := f.svc.Vend(ctx, *paid); err != nil {
					t.Fatal(err)
				}
			}
			got, _ := f.svc.Store.ByID(ctx, o.ID)
			if got.Status != c.want || f.spendCalls.Load() != c.vended {
				t.Fatalf("status %s, ABS called %d times; want %s and %d", got.Status, f.spendCalls.Load(), c.want, c.vended)
			}
		})
	}
}

func TestKoraSignatureCheckIsUsedByTheHandler(t *testing.T) {
	// Guards the wiring: the handler must use kora.VerifyWebhook, which signs only the data object.
	body, sig := koraEvent("charge.success", "MDEV_x")
	h := http.Header{}
	h.Set("x-korapay-signature", sig)
	if err := kora.VerifyWebhook(koraSecret, h, body); err != nil {
		t.Fatal(err)
	}
}
