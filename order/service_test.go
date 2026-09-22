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

	"Bills-Backend/abs"
	"Bills-Backend/monnify"
)

// A small ABS catalog with one of everything, in ABS's real shape.
const catalogReply = `{"status":true,"text":"Data found","data":{
  "airtimeproviders":[{"name":"MTN Airtime","product_trackid":"MTNAIR6TFD","allowed_numbers":"0803"}],
  "data_providers":[{"name":"MTN Data","product_trackid":"MTN56TFD","allowed_numbers":"0803","bouquetdata":[
    {"trackid":"P1","name":"1GB Daily","sellatwhatpercentoff":"290.00","recipient_type":"phone","validity_type":"Daily","validity":"1 day","validity_days":1},
    {"trackid":"F1","name":"FibreX 50Mbps","sellatwhatpercentoff":"29000.00","recipient_type":"fn","validity_type":"FibreX / ODU","validity_days":30},
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
	absStatus    atomic.Int32 // HTTP status ABS answers spend calls with
	absBody      atomic.Value
	meterOK      atomic.Bool

	mu        sync.Mutex
	spendPath string
	spendForm url.Values
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	f := &fakes{}
	f.invoiceState.Store("PENDING")
	f.absStatus.Store(200)
	f.absBody.Store(`{"status":true,"text":"Successful","data":{}}`)
	f.meterOK.Store(true)

	monnifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoice := func() string {
			return `{"requestSuccessful":true,"responseBody":{"amount":` + fmt.Sprintf("%.2f", float64(f.invoiceKobo.Load())/100) +
				`,"invoiceReference":"REF","invoiceStatus":"` + f.invoiceState.Load().(string) +
				`","expiryDate":"2030-01-02 13:00:00","accountNumber":"5000588061","bankName":"Moniepoint","transactionReference":"MNFY|` + fmt.Sprint(f.invoicesMade.Load()) + `"}}`
		}
		switch {
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
	}
	return f
}

// payAndVend takes an order through payment and purchase, like the webhooks do.
func (f *fakes) payAndVend(t *testing.T, o Order) Order {
	t.Helper()
	ctx := context.Background()
	f.invoiceState.Store("PAID")
	paid, err := f.svc.ConfirmPayment(ctx, o.TxnRef)
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
	if _, err := f.svc.ValidateMeter(context.Background(), "NOPE", "12345678901"); err == nil {
		t.Fatal("unknown service accepted")
	}

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
	if paid, err := f.svc.ConfirmPayment(ctx, o.TxnRef); err != nil || paid != nil {
		t.Fatalf("unpaid invoice must not be vended: %v %v", paid, err)
	}

	f.invoiceState.Store("PAID")
	paid, err := f.svc.ConfirmPayment(ctx, o.TxnRef)
	if err != nil || paid == nil {
		t.Fatalf("paid invoice should be confirmed: %v %v", paid, err)
	}
	if again, _ := f.svc.ConfirmPayment(ctx, o.TxnRef); again != nil {
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
