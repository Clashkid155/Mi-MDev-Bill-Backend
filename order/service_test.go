package order

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/email"
	"Bills-Backend/monnify"
)

// A small ABS catalog with one of everything, in ABS's real shape.
const catalogReply = `{"status":true,"text":"Data found","data":{
  "airtimeproviders":[{"name":"MTN Airtime","product_trackid":"MTNAIR6TFD","allowed_numbers":"0803","sellatwhatpercentoff":"99.00"}],
  "data_providers":[{"name":"MTN Data","product_trackid":"MTN56TFD","allowed_numbers":"0803","bouquetdata":[
    {"trackid":"P1","name":"1GB Daily","sellatwhatpercentoff":"290.00","recipient_type":"phone","validity_type":"Daily","validity":"1 day","validity_days":1},
    {"trackid":"F1","name":"FibreX 50Mbps","sellatwhatpercentoff":"29000.00","recipient_type":"fn","validity_type":"FibreX / ODU","validity_days":30},
    {"trackid":"CHEAP","name":"40MB","sellatwhatpercentoff":"48.00","recipient_type":"phone","validity_type":"Daily","validity_days":1},
    {"trackid":"NOTED","name":"600MB 2 Days Hot Deal","sellatwhatpercentoff":"250.00","note":"wont credit if user has an outstanding debt","recipient_type":"phone","validity_type":"Daily","validity_days":2},
    {"trackid":"FREE","name":"Unpriced","sellatwhatpercentoff":"0.00","recipient_type":"phone","validity_type":"Daily"}
  ]}],
  "electricity_providers":[{"name":"Oyo","trackid":"R1","servicedata":[{"trackid":"ELEC1","name":"IBEDC (prepaid)"}]}]
}}`

type fakes struct {
	svc          *Service
	invoicesMade atomic.Int32
	spendCalls   atomic.Int32
	meterCalls   atomic.Int32
	invoiceState atomic.Value // "PENDING" or "PAID"
	invoiceKobo  atomic.Int64 // amount of the invoice Monnify was asked for
	paidKobo     atomic.Int64 // what the customer actually paid, if not the invoice amount
	absStatus    atomic.Int32 // HTTP status ABS answers spend calls with
	absBody      atomic.Value
	meterOK      atomic.Bool
	profileDown  atomic.Bool // makes ABS's profile (balance) call fail

	emailsSent atomic.Int32
	emailOK    atomic.Bool // whether the fake ZeptoMail server accepts the message

	mu        sync.Mutex
	spendPath string
	spendForm url.Values
	lastEmail email.Message
	allEmails []email.Message // every message the fake ZeptoMail accepted, in order
}

func newFakes(t testing.TB) *fakes {
	t.Helper()
	f := &fakes{}
	f.invoiceState.Store("PENDING")
	f.absStatus.Store(200)
	f.absBody.Store(`{"status":true,"text":"Successful","data":{}}`)
	f.meterOK.Store(true)
	f.emailOK.Store(true)

	mailServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			To []struct {
				EmailAddress struct {
					Address string `json:"address"`
					Name    string `json:"name"`
				} `json:"email_address"`
			} `json:"to"`
			Subject  string `json:"subject"`
			TextBody string `json:"textbody"`
			HTMLBody string `json:"htmlbody"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.emailsSent.Add(1)
		if len(body.To) > 0 {
			f.mu.Lock()
			f.lastEmail = email.Message{
				To: body.To[0].EmailAddress.Address, ToName: body.To[0].EmailAddress.Name,
				Subject: body.Subject, Text: body.TextBody, HTML: body.HTMLBody,
			}
			if f.emailOK.Load() { // a message the fake refuses was not delivered
				f.allEmails = append(f.allEmails, f.lastEmail)
			}
			f.mu.Unlock()
		}
		if f.emailOK.Load() {
			w.Write([]byte(`{"data":[{"code":"EM_104","message":"OK"}],"message":"OK","request_id":"req-1"}`))
		} else {
			w.WriteHeader(502)
		}
	}))
	t.Cleanup(mailServer.Close)

	monnifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoice := func() string {
			return `{"requestSuccessful":true,"responseBody":{"amount":` + fmt.Sprintf("%.2f", float64(f.invoiceKobo.Load())/100) +
				`,"invoiceReference":"REF","invoiceStatus":"` + f.invoiceState.Load().(string) +
				`","expiryDate":"2030-01-02 13:00:00","accountNumber":"5000588061","bankName":"Moniepoint","transactionReference":"MNFY|` + fmt.Sprint(f.invoicesMade.Load()) + `"}}`
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v2/transactions/"):
			state := f.invoiceState.Load().(string)
			paid := f.paidKobo.Load() // 0 means: exactly what was asked, once the state is PAID
			if paid == 0 && state == "PAID" {
				paid = f.invoiceKobo.Load()
			}
			w.Write([]byte(fmt.Sprintf(`{"requestSuccessful":true,"responseBody":{"transactionReference":"T","paymentReference":"REF","paymentStatus":"%s","amountPaid":%.2f,"totalPayable":%.2f}}`,
				state, float64(paid)/100, float64(f.invoiceKobo.Load())/100)))
		case r.URL.Path == "/api/v1/auth/login":
			w.Write([]byte(`{"requestSuccessful":true,"responseBody":{"accessToken":"t","expiresIn":3600}}`))
		case r.URL.Path == "/api/v1/invoice/create":
			f.invoicesMade.Add(1)
			var in struct {
				Amount float64 `json:"amount"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			f.invoiceKobo.Store(int64(in.Amount*100 + 0.5))
			w.Write([]byte(invoice()))
		default: // GET invoice details
			w.Write([]byte(invoice()))
		}
	}))
	t.Cleanup(monnifyServer.Close)

	absServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.URL.Path {
		case "/api/user/system/get_system_data.php":
			w.Write([]byte(catalogReply))
		case "/api/user/profile/get_user_detail.php":
			if f.profileDown.Load() {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(`{"status":true,"text":"Data found","data":{"email":"owner@example.com","apikey":"************************",
				"user_wallets":[{"currencytag":"NGNT55","name":"NGN","walletbal":"726.00","walletpendbal":"0.00"}],"cashbackbal":"4274.00"}}`))
		case "/api/user/transaction/validate_meter.php":
			f.meterCalls.Add(1)
			if f.meterOK.Load() {
				w.Write([]byte(`{"status":true,"text":"ok","data":{"name":"JOHN DOE"}}`))
			} else {
				w.Write([]byte(`{"status":false,"text":"Meter not found"}`))
			}
		default:
			f.spendCalls.Add(1)
			f.mu.Lock()
			f.spendPath, f.spendForm = r.URL.Path, r.Form
			f.mu.Unlock()
			w.WriteHeader(int(f.absStatus.Load()))
			w.Write([]byte(f.absBody.Load().(string)))
		}
	}))
	t.Cleanup(absServer.Close)

	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	f.svc = &Service{
		Store:  store,
		Pay:    monnify.New(monnifyServer.URL, "k", "s", "CC"),
		Vendor: abs.New(absServer.URL, "key", false),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Sandbox is off so the fake server above actually receives the message.
		Mailer: email.New(email.Settings{BaseURL: mailServer.URL, Token: "tok", FromEmail: "orders@mi.app", FromName: "Mi MDev"}),
	}
	return f
}

// payAndVend takes an order through payment and purchase, like the webhooks do.
func (f *fakes) payAndVend(t *testing.T, o Order) Order {
	t.Helper()
	ctx := context.Background()
	f.invoiceState.Store("PAID")
	paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef)
	if err != nil || paid == nil {
		t.Fatalf("payment not confirmed: %v %v", paid, err)
	}
	if err := f.svc.Vend(ctx, *paid); err != nil {
		t.Fatal(err)
	}
	final, err := f.svc.Store.ByID(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	return final
}

var airtimeRequest = CreateRequest{
	IdempotencyKey: "key-1", Kind: KindAirtime, Network: "MTNAIR6TFD",
	Recipient: "08012345678", AmountKobo: 100_00, Email: "a@b.co",
}

func TestCreateIsIdempotent(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()

	first, err := f.svc.Create(ctx, airtimeRequest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Create(ctx, airtimeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || f.invoicesMade.Load() != 1 {
		t.Fatalf("retry must reuse the order: ids %s/%s, invoices %d", first.ID, second.ID, f.invoicesMade.Load())
	}
	if first.Status != StatusAwaitingPayment || first.AccountNo != "5000588061" {
		t.Fatalf("unexpected order %+v", first)
	}
}

func TestCreateValidation(t *testing.T) {
	f := newFakes(t)
	elec := CreateRequest{Kind: KindElectricity, ServiceID: "ELEC1", Recipient: "12345678901", AmountKobo: 1000_00}
	cases := map[string]CreateRequest{
		"no key":                  {Kind: KindAirtime},
		"bad email":               {IdempotencyKey: "k", Email: "nope", Kind: KindAirtime},
		"unknown kind":            {IdempotencyKey: "k", Email: "a@b.co", Kind: "cable"},
		"airtime unknown network": {Network: "NOPE", Recipient: "08012345678", AmountKobo: 100_00},
		"airtime below minimum":   {Network: "MTNAIR6TFD", Recipient: "08012345678", AmountKobo: 10_00},
		"airtime fractional":      {Network: "MTNAIR6TFD", Recipient: "08012345678", AmountKobo: 100_50},
		"airtime bad phone":       {Network: "MTNAIR6TFD", Recipient: "8012345678", AmountKobo: 100_00},
		"data unknown plan":       {PlanID: "NOPE", Recipient: "08012345678"},
		"data plan without price": {PlanID: "FREE", Recipient: "08012345678"},
		"data phone plan, FN":     {PlanID: "P1", Recipient: "FN111700"},
		"fibre plan, phone":       {PlanID: "F1", Recipient: "08012345678"},
		"fibre plan, short FN":    {PlanID: "F1", Recipient: "FN1117"},
		"electricity unknown":     {ServiceID: "NOPE", Recipient: elec.Recipient, AmountKobo: elec.AmountKobo},
		"electricity bad meter":   {ServiceID: "ELEC1", Recipient: "12ab", AmountKobo: elec.AmountKobo},
		"electricity below min":   {ServiceID: "ELEC1", Recipient: elec.Recipient, AmountKobo: 500_00},
	}
	kinds := map[string]string{
		"airtime": KindAirtime, "data": KindData, "electricity": KindElectricity,
	}
	for name, r := range cases {
		if r.IdempotencyKey == "" && !strings.HasPrefix(name, "no key") {
			r.IdempotencyKey, r.Email = "k-"+name, "a@b.co"
		}
		if r.Kind == "" {
			r.Kind = kinds[strings.Fields(name)[0]]
		}
		_, err := f.svc.Create(context.Background(), r)
		if _, ok := err.(InvalidError); !ok {
			t.Errorf("%s: want InvalidError, got %v", name, err)
		}
	}
	if f.invoicesMade.Load() != 0 || f.spendCalls.Load() != 0 {
		t.Fatalf("invalid requests must never reach Monnify or ABS: invoices=%d spends=%d", f.invoicesMade.Load(), f.spendCalls.Load())
	}
}

// Order creation no longer calls ABS's validate-meter endpoint: the app is
// expected to have called POST /v1/meters/validate itself and shown the
// customer the owner before this request is ever sent.
func TestElectricityOrderDoesNotCallValidateMeter(t *testing.T) {
	f := newFakes(t)
	f.meterOK.Store(false) // even a meter ABS would reject must not block Create
	o, err := f.svc.Create(context.Background(), CreateRequest{
		IdempotencyKey: "e1", Kind: KindElectricity, Email: "a@b.co",
		ServiceID: "ELEC1", Recipient: "12345678901", AmountKobo: 1000_00,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.meterCalls.Load() != 0 {
		t.Fatalf("Create must not call ABS's validate-meter endpoint, got %d calls", f.meterCalls.Load())
	}
	if o.Status != StatusAwaitingPayment {
		t.Fatalf("order status %s", o.Status)
	}
}

// The standalone meter check (POST /v1/meters/validate) still works on its own.
func TestValidateMeter(t *testing.T) {
	f := newFakes(t)
	// The service id is not checked against our catalog here; ABS is the judge.
	f.meterOK.Store(false)
	_, err := f.svc.ValidateMeter(context.Background(), "ELEC1", "12345678901")
	invalid, ok := err.(InvalidError)
	if !ok || !strings.Contains(invalid.Message, "Meter not found") {
		t.Fatalf("want the meter rejected with ABS's reason, got %v", err)
	}

	f.meterOK.Store(true)
	details, err := f.svc.ValidateMeter(context.Background(), "ELEC1", "12345678901")
	if err != nil || string(details) != `{"name":"JOHN DOE"}` {
		t.Fatalf("%v %s", err, details)
	}
}

// Each kind creates an invoice for the right amount and asks ABS for the right thing.
func TestEachKindIsPricedAndVended(t *testing.T) {
	cases := []struct {
		name       string
		req        CreateRequest
		wantKobo   int64
		wantPath   string
		wantFields map[string]string
	}{
		{
			"airtime",
			CreateRequest{Kind: KindAirtime, Network: "MTNAIR6TFD", Recipient: "08012345678", AmountKobo: 250_00},
			250_00, "/api/user/transaction/buy_airtime.php",
			map[string]string{"airtimetid": "MTNAIR6TFD", "phoneno": "08012345678", "amount": "250"},
		},
		{
			"data priced from the catalog, not the app",
			CreateRequest{Kind: KindData, PlanID: "P1", Recipient: "08012345678", AmountKobo: 1}, // app's amount is ignored
			290_00, "/api/user/transaction/buy_data.php",
			map[string]string{"airtimetid": "MTN56TFD", "cardunittid": "P1", "phoneno": "08012345678"},
		},
		{
			"fibre plan bought with an FN",
			CreateRequest{Kind: KindData, PlanID: "F1", Recipient: " fn111700 "},
			29000_00, "/api/user/transaction/buy_data.php",
			map[string]string{"airtimetid": "MTN56TFD", "cardunittid": "F1", "phoneno": "FN111700"},
		},
		{
			"electricity",
			CreateRequest{Kind: KindElectricity, ServiceID: "ELEC1", Recipient: "12345678901", AmountKobo: 2000_00},
			2000_00, "/api/user/transaction/buy_electricity.php",
			map[string]string{"provider_tid": "ELEC1", "meter_number": "12345678901", "amount": "2000"},
		},
	}
	for _, tc := range cases {
		f := newFakes(t)
		tc.req.IdempotencyKey, tc.req.Email = "k", "a@b.co"
		o, err := f.svc.Create(context.Background(), tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if o.AmountKobo != tc.wantKobo || f.invoiceKobo.Load() != tc.wantKobo {
			t.Errorf("%s: order %d kobo, invoice %d kobo, want %d", tc.name, o.AmountKobo, f.invoiceKobo.Load(), tc.wantKobo)
		}

		final := f.payAndVend(t, o)
		if final.Status != StatusSuccess {
			t.Errorf("%s: status %s", tc.name, final.Status)
		}
		f.mu.Lock()
		path, form := f.spendPath, f.spendForm
		f.mu.Unlock()
		if path != tc.wantPath {
			t.Errorf("%s: called %s, want %s", tc.name, path, tc.wantPath)
		}
		for field, want := range tc.wantFields {
			if got := form.Get(field); got != want {
				t.Errorf("%s: ABS field %s = %q, want %q", tc.name, field, got, want)
			}
		}
	}
}

func TestPaidOrderIsVendedOnce(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)

	// Webhook arrives but Monnify says the invoice is not actually paid.
	if paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef); err != nil || paid != nil {
		t.Fatalf("unpaid invoice must not be vended: %v %v", paid, err)
	}

	f.invoiceState.Store("PAID")
	paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef)
	if err != nil || paid == nil {
		t.Fatalf("paid invoice should be confirmed: %v %v", paid, err)
	}
	if again, _ := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef); again != nil {
		t.Fatal("duplicate webhook must not confirm twice")
	}

	if err := f.svc.Vend(ctx, *paid); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Vend(ctx, *paid); err != nil { // e.g. a racing worker
		t.Fatal(err)
	}
	if f.spendCalls.Load() != 1 {
		t.Fatalf("ABS must be called exactly once, got %d", f.spendCalls.Load())
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusSuccess {
		t.Fatalf("status %s", got.Status)
	}
}

// A wrong amount is never vended, and the order's status says what actually
// happened: PARTIALLY_PAID or OVERPAID, not left as AWAITING_PAYMENT.
func TestWrongAmountIsRecordedAsWhatItIsAndNeverVended(t *testing.T) {
	cases := []struct {
		name, state string
		paidKobo    int64
		want        Status
	}{
		{"underpaid", "PARTIALLY_PAID", 50_00, StatusPartiallyPaid},
		{"overpaid", "OVERPAID", 150_00, StatusOverpaid},
		{"labelled paid but short", "PAID", 99_00, StatusPartiallyPaid},
		{"labelled paid but over", "PAID", 101_00, StatusOverpaid},
	}
	for _, tc := range cases {
		f := newFakes(t)
		ctx := context.Background()
		o, _ := f.svc.Create(ctx, airtimeRequest) // ₦100
		f.invoiceState.Store(tc.state)
		f.paidKobo.Store(tc.paidKobo)
		if paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef); err != nil || paid != nil {
			t.Errorf("%s: must not be confirmed: %v %v", tc.name, paid, err)
		}
		if f.spendCalls.Load() != 0 {
			t.Errorf("%s: ABS was called", tc.name)
		}
		if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != tc.want {
			t.Errorf("%s: status %s, want %s", tc.name, got.Status, tc.want)
		}
	}
}

// A short payment topped up to the exact amount is still vended.
func TestPartiallyPaidCanBeToppedUpToPaid(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest) // ₦100
	f.invoiceState.Store("PARTIALLY_PAID")
	f.paidKobo.Store(50_00)
	f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef)
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusPartiallyPaid {
		t.Fatalf("status %s", got.Status)
	}

	f.invoiceState.Store("PAID")
	f.paidKobo.Store(0) // 0 means "exactly what was invoiced" (see the fake server)
	paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef)
	if err != nil || paid == nil || paid.Status != StatusPaid {
		t.Fatalf("%v %v", paid, err)
	}
}

// An order whose webhook never arrived still moves on when the app polls it.
func TestPollingConfirmsAPaymentTheWebhookMissed(t *testing.T) {
	f := newFakes(t)
	api := &API{Service: f.svc}
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()
	o, _ := f.svc.Create(context.Background(), airtimeRequest)

	status := func() Status {
		resp, err := http.Get(srv.URL + "/v1/orders/" + o.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var env struct {
			Data struct{ Status Status } `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&env)
		return env.Data.Status
	}
	if got := status(); got != StatusAwaitingPayment {
		t.Fatalf("unpaid order: %s", got)
	}
	f.invoiceState.Store("PAID")
	api.lastChecked.Delete(o.ID) // skip the throttle wait
	status()
	api.Wait()
	if got, _ := f.svc.Store.ByID(context.Background(), o.ID); got.Status != StatusSuccess || f.spendCalls.Load() != 1 {
		t.Fatalf("status %s, ABS calls %d", got.Status, f.spendCalls.Load())
	}
}

func TestCustomerName(t *testing.T) {
	if got := customerName("  Ada Obi ", "ada@x.co"); got != "Ada Obi" {
		t.Errorf("given name: %q", got)
	}
	for _, blank := range []string{"", "   "} {
		if got := customerName(blank, "ada.obi@x.co"); got != "ada.obi" {
			t.Errorf("default for %q: %q", blank, got)
		}
	}
	r := airtimeRequest
	r.CustomerName = strings.Repeat("a", maxCustomerName+1)
	if _, ok := r.validate().(InvalidError); !ok {
		t.Error("over-long name accepted")
	}
}

// The resolved customer name is stored on the order, not just used once for
// the invoice, so it is still available later (e.g. for the receipt email).
func TestCustomerNameIsPersisted(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()

	given := airtimeRequest
	given.IdempotencyKey, given.CustomerName = "k-given", "Ada Obi"
	o, err := f.svc.Create(ctx, given)
	if err != nil {
		t.Fatal(err)
	}
	if o.CustomerName != "Ada Obi" {
		t.Errorf("given name: %q", o.CustomerName)
	}
	if stored, _ := f.svc.Store.ByID(ctx, o.ID); stored.CustomerName != "Ada Obi" {
		t.Errorf("not persisted: %q", stored.CustomerName)
	}

	defaulted := airtimeRequest
	defaulted.IdempotencyKey, defaulted.Email = "k-default", "priya@x.co"
	o2, err := f.svc.Create(ctx, defaulted)
	if err != nil {
		t.Fatal(err)
	}
	if o2.CustomerName != "priya" {
		t.Errorf("default name: %q", o2.CustomerName)
	}
}

func TestAdminWebhookList(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	ctx := context.Background()
	f.svc.Store.LogWebhook(ctx, "monnify", "T1", []byte(`{"eventType":"SUCCESSFUL_TRANSACTION"}`))
	f.svc.Store.LogWebhook(ctx, "abs", "D1", []byte(`not json`))

	get := func(query string) string {
		resp, err := http.Get(srv.URL + "/admin/webhooks" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if body := get("?provider=monnify"); !strings.Contains(body, `"count":1`) || !strings.Contains(body, `"body":{"eventType":"SUCCESSFUL_TRANSACTION"}`) {
		t.Errorf("monnify only, body as JSON: %s", body)
	}
	if body := get(""); !strings.Contains(body, `"count":2`) || !strings.Contains(body, `"body":"not json"`) {
		t.Errorf("all, non-JSON kept as text: %s", body)
	}
}

func TestAdminOrderList(t *testing.T) {
	f := newFakes(t)
	api := &API{Service: f.svc}
	srv := httptest.NewServer(api.AdminRoutes())
	defer srv.Close()
	ctx := context.Background()
	a, _ := f.svc.Create(ctx, airtimeRequest)
	second := airtimeRequest
	second.IdempotencyKey = "key-2"
	f.svc.Create(ctx, second)
	f.payAndVend(t, a)

	list := func(query string) (int, int) {
		resp, err := http.Get(srv.URL + "/admin/orders" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var env struct {
			Data struct{ Count int } `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&env)
		return resp.StatusCode, env.Data.Count
	}
	if code, n := list(""); code != 200 || n != 2 {
		t.Errorf("all: %d %d", code, n)
	}
	if code, n := list("?status=success"); code != 200 || n != 1 {
		t.Errorf("success: %d %d", code, n)
	}
	if code, n := list("?status=AWAITING_PAYMENT,SUCCESS&limit=1"); code != 200 || n != 1 {
		t.Errorf("limit: %d %d", code, n)
	}
	if code, _ := list("?status=NOPE"); code != 400 {
		t.Errorf("bad status accepted: %d", code)
	}
}

// expire makes an order's payment window end an hour ago and returns it as stored.
func (f *fakes) expire(t *testing.T, o Order) Order {
	t.Helper()
	if _, err := f.svc.Store.db.Exec(`UPDATE orders SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Hour).Unix(), o.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Store.ByID(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestUnpaidOrderExpiresAfterItsWindowAndIsNeverVended(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)

	// Still inside the window: left alone.
	if paid, err := f.svc.Settle(ctx, o); err != nil || paid != nil {
		t.Fatalf("%v %v", paid, err)
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusAwaitingPayment {
		t.Fatalf("expired too early: %s", got.Status)
	}

	late := f.expire(t, o)
	if paid, err := f.svc.Settle(ctx, late); err != nil || paid != nil {
		t.Fatalf("%v %v", paid, err)
	}
	got, _ := f.svc.Store.ByID(ctx, o.ID)
	if got.Status != StatusExpired || f.spendCalls.Load() != 0 {
		t.Fatalf("status %s, ABS calls %d", got.Status, f.spendCalls.Load())
	}
	if viewOf(got).Payment != nil {
		t.Fatal("an expired order must not offer its account number")
	}
}

// Monnify is asked before an order is expired, so a payment made in time whose
// webhook was slow is vended, not expired.
func TestPaymentMadeInTimeBeatsTheDeadline(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	late := f.expire(t, o)
	f.invoiceState.Store("PAID")

	paid, err := f.svc.Settle(ctx, late)
	if err != nil || paid == nil || paid.Status != StatusPaid {
		t.Fatalf("paid order must be confirmed, got %v %v", paid, err)
	}
}

// A transfer that lands on an already-expired order is never vended: it is flagged for refund.
func TestLatePaymentOnExpiredOrderIsRefundNotVended(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	f.svc.Settle(ctx, f.expire(t, o))
	f.invoiceState.Store("PAID")

	if paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef); err != nil || paid != nil {
		t.Fatalf("expired order must not be vended: %v %v", paid, err)
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusRefundDue || f.spendCalls.Load() != 0 {
		t.Fatalf("status %s, ABS calls %d", got.Status, f.spendCalls.Load())
	}
}

func TestSweepExpiresOnlyOrdersPastTheirWindow(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	stale, _ := f.svc.Create(ctx, airtimeRequest)
	second := airtimeRequest
	second.IdempotencyKey = "key-2"
	fresh, _ := f.svc.Create(ctx, second)
	f.expire(t, stale)

	(&API{Service: f.svc}).sweepOnce(ctx)
	if got, _ := f.svc.Store.ByID(ctx, stale.ID); got.Status != StatusExpired {
		t.Errorf("stale order: %s", got.Status)
	}
	if got, _ := f.svc.Store.ByID(ctx, fresh.ID); got.Status != StatusAwaitingPayment {
		t.Errorf("fresh order: %s", got.Status)
	}
}

// Orders are found by our own reference (Monnify's paymentReference) as well as
// by its transaction reference, so a webhook that arrives before we saved the
// transaction reference still finds its order.
func TestPaymentIsMatchedByOrderReference(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	f.svc.Store.db.Exec(`UPDATE orders SET txn_ref = '' WHERE id = ?`, o.ID)
	f.invoiceState.Store("PAID")

	paid, err := f.svc.ConfirmPayment(ctx, o.ID, "MNFY|from-webhook")
	if err != nil || paid == nil || paid.ID != o.ID {
		t.Fatalf("not matched by order reference: %v %v", paid, err)
	}
}

func TestUnknownPaymentMatchesNothing(t *testing.T) {
	f := newFakes(t)
	if paid, err := f.svc.ConfirmPayment(context.Background(), "no-such-order", "MNFY|nope"); err != nil || paid != nil {
		t.Fatalf("%v %v", paid, err)
	}
}

func TestWebhookLogsIgnoredEventTypes(t *testing.T) {
	f := newFakes(t)
	var logs strings.Builder
	f.svc.Log = slog.New(slog.NewTextHandler(&logs, nil))
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhooks/monnify", "application/json",
		strings.NewReader(`{"eventType":"REJECTED_PAYMENT","eventData":{"paymentReference":"abc","transactionReference":"T1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(logs.String(), "ignoring REJECTED_PAYMENT event (order abc)") {
		t.Fatalf("status %d, log %q", resp.StatusCode, logs.String())
	}
}

// The same payment notification arriving many times at once (Monnify retries,
// plus the app polling) must still buy exactly once.
func TestConcurrentDuplicateNotificationsVendOnce(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	f.invoiceState.Store("PAID")

	var wg sync.WaitGroup
	var confirmed atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef)
			if err != nil {
				t.Error(err)
				return
			}
			if paid != nil {
				confirmed.Add(1)
				if err := f.svc.Vend(ctx, *paid); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if confirmed.Load() != 1 || f.spendCalls.Load() != 1 {
		t.Fatalf("confirmed %d times, ABS called %d times; want 1 and 1", confirmed.Load(), f.spendCalls.Load())
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusSuccess {
		t.Fatalf("status %s", got.Status)
	}
}

func TestVendOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		status int32
		body   string
		want   Status
	}{
		{"rejected by ABS", 200, `{"status":false,"text":"no funds"}`, StatusRefundDue},
		{"ABS down", 502, `bad gateway`, StatusPending},
	}
	for _, tc := range cases {
		f := newFakes(t)
		f.absStatus.Store(tc.status)
		f.absBody.Store(tc.body)
		o, _ := f.svc.Create(context.Background(), airtimeRequest)
		if got := f.payAndVend(t, o); got.Status != tc.want {
			t.Errorf("%s: status %s, want %s", tc.name, got.Status, tc.want)
		}
	}
}

// The electricity token is handed to the app only once the order succeeded.
func TestDeliveryIsOnlyShownOnSuccess(t *testing.T) {
	f := newFakes(t)
	f.absBody.Store(`{"status":true,"text":"Successful","data":{"token":"1234-5678-9012"}}`)
	o, err := f.svc.Create(context.Background(), CreateRequest{
		IdempotencyKey: "e1", Kind: KindElectricity, Email: "a@b.co",
		ServiceID: "ELEC1", Recipient: "12345678901", AmountKobo: 1000_00,
	})
	if err != nil {
		t.Fatal(err)
	}
	if viewOf(o).Delivery != nil {
		t.Fatal("nothing may be delivered before payment")
	}
	final := f.payAndVend(t, o)
	if got := string(viewOf(final).Delivery); got != `{"token":"1234-5678-9012"}` {
		t.Fatalf("delivery = %s", got)
	}

	// Airtime has nothing to hand over.
	air, _ := f.svc.Create(context.Background(), airtimeRequest)
	if viewOf(f.payAndVend(t, air)).Delivery != nil {
		t.Fatal("airtime orders have no delivery")
	}
}

// Display names are copied onto the order when it is created, so later catalog
// changes cannot rewrite what a past order says it bought.
func TestProductNamesAreSnapshottedAtCreation(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	cases := []struct {
		req           CreateRequest
		network, plan string
	}{
		{CreateRequest{Kind: KindAirtime, Network: "MTNAIR6TFD", Recipient: "08012345678", AmountKobo: 100_00}, "MTN Airtime", ""},
		{CreateRequest{Kind: KindData, PlanID: "P1", Recipient: "08012345678"}, "MTN Data", "1GB Daily"},
		{CreateRequest{Kind: KindElectricity, ServiceID: "ELEC1", Recipient: "12345678901", AmountKobo: 1000_00}, "IBEDC (prepaid)", ""},
	}
	for i, tc := range cases {
		tc.req.IdempotencyKey, tc.req.Email = fmt.Sprintf("names-%d", i), "a@b.co"
		o, err := f.svc.Create(ctx, tc.req)
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := f.svc.Store.ByID(ctx, o.ID)
		if stored.NetworkName != tc.network || stored.PlanName != tc.plan {
			t.Errorf("%s: network %q plan %q, want %q %q", tc.req.Kind, stored.NetworkName, stored.PlanName, tc.network, tc.plan)
		}
	}
}

// With a provider minimum (Kora: ₦100), airtime below it and data plans priced
// below it cannot be ordered, and those plans are left out of the catalog.
// With no minimum (the default) nothing changes.
func TestMinimumPayment(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	cheap := CreateRequest{Kind: KindData, PlanID: "CHEAP", Recipient: "08012345678", IdempotencyKey: "m1", Email: "a@b.co"}
	air50 := CreateRequest{Kind: KindAirtime, Network: "MTNAIR6TFD", Recipient: "08012345678", AmountKobo: 50_00, IdempotencyKey: "m2", Email: "a@b.co"}

	// No minimum: ₦50 airtime is fine (ABS's own floor) and so is the cheap plan.
	if _, err := f.svc.Create(ctx, air50); err != nil {
		t.Fatalf("no minimum, airtime of ₦50: %v", err)
	}
	if _, err := f.svc.Create(ctx, cheap); err != nil {
		t.Fatalf("no minimum, cheap plan: %v", err)
	}

	f.svc.MinPaymentKobo = 100_00
	air50.IdempotencyKey, cheap.IdempotencyKey = "m3", "m4"
	for name, req := range map[string]CreateRequest{"airtime of ₦50": air50, "plan under ₦100": cheap} {
		if _, err := f.svc.Create(ctx, req); err == nil {
			t.Errorf("%s accepted with a ₦100 minimum", name)
		} else if _, ok := err.(InvalidError); !ok {
			t.Errorf("%s: want InvalidError, got %v", name, err)
		}
	}
	ok := air50
	ok.IdempotencyKey, ok.AmountKobo = "m5", 100_00
	if _, err := f.svc.Create(ctx, ok); err != nil {
		t.Errorf("airtime of exactly ₦100 must be fine: %v", err)
	}

	// The catalog drops the cheap plan, keeps the rest, and never changes a price.
	full := abs.Catalog{Data: []abs.DataNetwork{{ID: "n", Categories: []abs.DataCategory{
		{Name: "Daily", Plans: []abs.DataPlan{{ID: "a", PriceKobo: 48_00}, {ID: "b", PriceKobo: 100_00}, {ID: "c", PriceKobo: 290_00}}},
		{Name: "Only cheap", Plans: []abs.DataPlan{{ID: "d", PriceKobo: 80_00}}},
	}}}}
	got := withinMinimumPayment(full, 100_00)
	if len(got.Data[0].Categories) != 1 || len(got.Data[0].Categories[0].Plans) != 2 ||
		got.Data[0].Categories[0].Plans[0].PriceKobo != 100_00 || got.Data[0].Categories[0].Plans[1].PriceKobo != 290_00 {
		t.Fatalf("filtered catalog: %+v", got.Data)
	}
	if len(full.Data[0].Categories) != 2 || len(full.Data[0].Categories[0].Plans) != 3 {
		t.Fatal("the original catalog must not be modified")
	}
	if same := withinMinimumPayment(full, 0); len(same.Data[0].Categories) != 2 {
		t.Fatal("no minimum must leave the catalog alone")
	}
}

// A successful order emails the customer once; other outcomes never do.
func TestReceiptEmailOnlyOnSuccess(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	r := airtimeRequest
	r.CustomerName = "Ada Obi"
	o, _ := f.svc.Create(ctx, r)

	final := f.payAndVend(t, o)
	if final.Status != StatusSuccess || f.emailsSent.Load() != 1 {
		t.Fatalf("status %s, emails %d", final.Status, f.emailsSent.Load())
	}
	f.mu.Lock()
	got := f.lastEmail
	f.mu.Unlock()
	if got.To != o.CustomerEmail || got.ToName != "Ada Obi" || !strings.Contains(got.Subject, "airtime") ||
		!strings.Contains(got.Text, "08012345678") || !strings.Contains(got.Text, "100.00") {
		t.Fatalf("email = %+v", got)
	}
	// The HTML version goes out alongside the plain one, with the snapshotted network name.
	for _, want := range []string{o.ID, "MTN Airtime", "08012345678", "₦100.00"} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("html body is missing %q", want)
		}
	}

	// A failed or pending vend never emails anyone.
	f.absStatus.Store(502)
	pending, _ := f.svc.Create(ctx, CreateRequest{IdempotencyKey: "k2", Kind: KindAirtime, Email: "a@b.co",
		Network: "MTNAIR6TFD", Recipient: "08012345678", AmountKobo: 100_00})
	f.payAndVend(t, pending)
	if f.emailsSent.Load() != 1 {
		t.Fatalf("a non-success vend must not email: %d sent", f.emailsSent.Load())
	}
}

// The electricity token is included in the receipt.
func TestReceiptEmailIncludesTheElectricityToken(t *testing.T) {
	f := newFakes(t)
	f.absBody.Store(`{"status":true,"text":"Successful","data":{"token":"1234-5678-9012"}}`)
	o, _ := f.svc.Create(context.Background(), CreateRequest{
		IdempotencyKey: "e1", Kind: KindElectricity, Email: "a@b.co", CustomerName: "Ada Obi",
		ServiceID: "ELEC1", Recipient: "12345678901", AmountKobo: 1000_00,
	})
	f.payAndVend(t, o)
	f.mu.Lock()
	got := f.lastEmail.Text
	f.mu.Unlock()
	if !strings.Contains(got, "1234-5678-9012") {
		t.Fatalf("token missing from receipt: %q", got)
	}
}

// Built from a real ABS electricity purchase reply, to guard against the exact
// shape (including its odd float precision on "unit") ever breaking extraction.
func TestReceiptEmailAgainstARealABSElectricityReply(t *testing.T) {
	f := newFakes(t)
	f.absBody.Store(`{
		"status": true,
		"text": "Successful",
		"data": {
			"token": "5296-7725-2448-4408-1616",
			"unit": 4.4000000000000003552713678800500929355621337890625
		},
		"time": "28-09-26 12:06:19PM",
		"method": "POST",
		"endpoint": "http:\/\/absvendor.com:443\/api\/user\/transaction\/buy_electricity.php",
		"error": []
	}`)
	o, err := f.svc.Create(context.Background(), CreateRequest{
		IdempotencyKey: "e-real", Kind: KindElectricity, Email: "a@b.co", CustomerName: "Ada Obi",
		ServiceID: "ELEC1", Recipient: "12345678901", AmountKobo: 1000_00,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := f.payAndVend(t, o)
	if final.Status != StatusSuccess {
		t.Fatalf("status %s", final.Status)
	}
	if got := string(viewOf(final).Delivery); !strings.Contains(got, "5296-7725-2448-4408-1616") {
		t.Fatalf("delivery = %s", got)
	}
	f.mu.Lock()
	got := f.lastEmail.Text
	f.mu.Unlock()
	if !strings.Contains(got, "5296-7725-2448-4408-1616") {
		t.Fatalf("token missing from receipt: %q", got)
	}
}

// A broken or unconfigured mailer never fails the order itself.
func TestReceiptEmailFailureDoesNotFailTheOrder(t *testing.T) {
	f := newFakes(t)
	f.emailOK.Store(false)
	o, _ := f.svc.Create(context.Background(), airtimeRequest)
	if final := f.payAndVend(t, o); final.Status != StatusSuccess {
		t.Fatalf("order must still succeed when the mailer fails: %s", final.Status)
	}

	f2 := newFakes(t)
	f2.svc.Mailer = nil // no email provider configured
	o2, _ := f2.svc.Create(context.Background(), airtimeRequest)
	if final := f2.payAndVend(t, o2); final.Status != StatusSuccess {
		t.Fatalf("order must succeed with no mailer configured: %s", final.Status)
	}
}

func TestOldDatabasesGetTheNewColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.db.Exec(`ALTER TABLE orders DROP COLUMN plan_id`)
	store.Close()

	store, err = OpenStore(path) // must add it back rather than fail
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	o := Order{IdempotencyKey: "k", Kind: KindData, Network: "n", PlanID: "p", Recipient: "r", AmountKobo: 1, CustomerEmail: "a@b.co"}
	if err := store.Insert(context.Background(), &o); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.ByID(context.Background(), o.ID); got.PlanID != "p" {
		t.Fatalf("plan id not stored: %+v", got)
	}
}
