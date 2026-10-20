package order

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var dataRequest = CreateRequest{IdempotencyKey: "d1", Kind: KindData, PlanID: "P1", Recipient: "08012345678", Email: "a@b.co"}

var electricityRequest = CreateRequest{IdempotencyKey: "e1", Kind: KindElectricity, ServiceID: "ELEC1", Recipient: "12345678901",
	AmountKobo: 1_000_00, Email: "a@b.co"}

func TestMarkupSetsThePriceAndKeepsTheCost(t *testing.T) {
	f := newFakes(t)
	f.svc.Pricing = realPricing()
	ctx := context.Background()

	o, err := f.svc.Create(ctx, dataRequest) // ABS charges ₦290; MTN 2.5% makes 297.25, rounded up to ₦300
	if err != nil {
		t.Fatal(err)
	}
	if o.AmountKobo != 300_00 || o.CostKobo != 290_00 || o.TotalKobo != 300_00 || o.FeeKobo != 0 {
		t.Fatalf("order: amount %d cost %d total %d fee %d", o.AmountKobo, o.CostKobo, o.TotalKobo, o.FeeKobo)
	}
	if f.invoiceKobo.Load() != 300_00 {
		t.Fatalf("the customer must be asked for the marked-up price, got %d", f.invoiceKobo.Load())
	}

	// The catalog shows the same price, and never the cost.
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/catalog")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var reply struct {
		Data struct {
			Data []struct {
				Categories []struct {
					Plans []struct {
						ID    string `json:"id"`
						Price int64  `json:"price_kobo"`
					} `json:"plans"`
				} `json:"categories"`
			} `json:"data"`
			ElectricityFee feeRule `json:"electricity_fee"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	var shown int64
	for _, n := range reply.Data.Data {
		for _, c := range n.Categories {
			for _, p := range c.Plans {
				if p.ID == "P1" {
					shown = p.Price
				}
			}
		}
	}
	if shown != 300_00 {
		t.Errorf("the catalog shows %d for P1, want 30000", shown)
	}
	if strings.Contains(strings.ToLower(string(body)), "cost") {
		t.Errorf("the vendor cost must never reach the app: %s", body)
	}
	if reply.Data.ElectricityFee.MinKobo != 50_00 || reply.Data.ElectricityFee.Percent != 1.8 {
		t.Errorf("the catalog should publish the electricity fee rule: %+v", reply.Data.ElectricityFee)
	}

	// Paid and delivered: the order earned the markup.
	final := f.payAndVend(t, o)
	if profit := final.Profit(); profit == nil || *profit != 10_00 {
		t.Fatalf("profit: %v", profit)
	}
}

func TestHiddenPlansAreNotOffered(t *testing.T) {
	f := newFakes(t)
	f.svc.Pricing = realPricing()
	f.svc.Pricing.Hidden = map[string]bool{"P1": true}
	_, err := f.svc.Create(context.Background(), dataRequest)
	var invalid InvalidError
	if !errors.As(err, &invalid) || !strings.Contains(invalid.Message, "unknown data plan") {
		t.Fatalf("a hidden plan must be refused like an unknown one, got %v", err)
	}
	catalog, _ := f.svc.SellCatalog(context.Background())
	if _, _, ok := catalog.FindDataPlan("P1"); ok {
		t.Error("a hidden plan must not appear in the catalog")
	}
}

func TestAirtimeCostFollowsTheNetworkDiscount(t *testing.T) {
	f := newFakes(t)
	f.svc.Pricing = realPricing()
	req := airtimeRequest
	req.AmountKobo = 1_000_00
	o, err := f.svc.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Airtime is sold at face value; ABS charges 99 per 100.
	if o.AmountKobo != 1_000_00 || o.TotalKobo != 1_000_00 || o.CostKobo != 990_00 || o.FeeKobo != 0 {
		t.Fatalf("order: amount %d cost %d total %d fee %d", o.AmountKobo, o.CostKobo, o.TotalKobo, o.FeeKobo)
	}
}

func TestElectricityFeeIsPaidByTheCustomerAndNeverSentToABS(t *testing.T) {
	f := newFakes(t)
	f.svc.Pricing = realPricing()
	ctx := context.Background()

	o, err := f.svc.Create(ctx, electricityRequest) // ₦1,000 token: fee ₦50, total ₦1,050
	if err != nil {
		t.Fatal(err)
	}
	if o.AmountKobo != 1_000_00 || o.FeeKobo != 50_00 || o.TotalKobo != 1_050_00 || o.CostKobo != 1_000_00 {
		t.Fatalf("order: amount %d fee %d total %d cost %d", o.AmountKobo, o.FeeKobo, o.TotalKobo, o.CostKobo)
	}
	if f.invoiceKobo.Load() != 1_050_00 {
		t.Fatalf("the customer is asked for the total, got %d", f.invoiceKobo.Load())
	}
	if v := viewOf(o); v.AmountKobo != 1_000_00 || v.FeeKobo != 50_00 || v.TotalKobo != 1_050_00 {
		t.Errorf("the app is told the amount, the fee and the total: %+v", v)
	}

	// Paying just the token amount is a short payment, not a purchase.
	f.invoiceState.Store("PAID")
	f.paidKobo.Store(1_000_00)
	if paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef); err != nil || paid != nil {
		t.Fatalf("paying without the fee must not be accepted: %v %v", paid, err)
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusPartiallyPaid || f.spendCalls.Load() != 0 {
		t.Fatalf("status %s, ABS called %d times", got.Status, f.spendCalls.Load())
	}

	// Paying the total buys the token, for the amount only.
	f.paidKobo.Store(0)
	final := f.payAndVend(t, o)
	if final.Status != StatusSuccess {
		t.Fatalf("status %s", final.Status)
	}
	f.mu.Lock()
	sent := f.spendForm.Get("amount")
	f.mu.Unlock()
	if sent != "1000" {
		t.Errorf("ABS must be asked for the 1000 naira token, not the fee, got %q", sent)
	}
	if profit := final.Profit(); profit == nil || *profit != 50_00 {
		t.Errorf("the fee is the profit before payment costs: %v", profit)
	}

	// The receipt shows the fee and the total.
	f.mu.Lock()
	mail := f.lastEmail
	f.mu.Unlock()
	for _, want := range []string{"Service fee: ₦50.00", "Total paid: ₦1,050.00"} {
		if !strings.Contains(mail.Text, want) {
			t.Errorf("receipt text is missing %q:\n%s", want, mail.Text)
		}
	}
	if !strings.Contains(mail.HTML, "Service fee") || !strings.Contains(mail.HTML, "₦1,050.00") {
		t.Errorf("receipt HTML is missing the fee or the total")
	}
}

func TestOrdersMadeBeforeTotalsWereKeptStillWork(t *testing.T) {
	old := Order{AmountKobo: 100_00, Status: StatusSuccess} // total 0, cost 0, as before
	if old.Due() != 100_00 {
		t.Errorf("an old order is due its amount, got %d", old.Due())
	}
	if old.Profit() != nil {
		t.Error("profit without a recorded cost would be invented")
	}
	if v := viewOf(old); v.TotalKobo != 100_00 || v.FeeKobo != 0 {
		t.Errorf("view: %+v", v)
	}
	paid := Order{AmountKobo: 1_000_00, FeeKobo: 50_00, TotalKobo: 1_050_00, CostKobo: 1_000_00, PaymentFeeKobo: 16_93, Status: StatusSuccess}
	if p := paid.Profit(); p == nil || *p != 50_00-16_93 {
		t.Errorf("profit: %v", p)
	}
	if (Order{Status: StatusRefundDue, AmountKobo: 1, CostKobo: 1}).Profit() != nil {
		t.Error("only a SUCCESS order has a profit")
	}
}

func TestOldDatabasesGetTheCostAndFeeColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"cost_kobo", "fee_kobo", "total_kobo", "payment_fee_kobo"} {
		if _, err := store.db.Exec(`ALTER TABLE orders DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()

	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	o := Order{IdempotencyKey: "k", Kind: KindElectricity, Network: "n", Recipient: "r", AmountKobo: 1_000_00, CostKobo: 1_000_00,
		FeeKobo: 50_00, TotalKobo: 1_050_00, CustomerEmail: "a@b.co", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Insert(context.Background(), &o); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaymentFee(context.Background(), o.ID, 16_93); err != nil {
		t.Fatal(err)
	}
	got, _ := store.ByID(context.Background(), o.ID)
	if got.CostKobo != 1_000_00 || got.FeeKobo != 50_00 || got.TotalKobo != 1_050_00 || got.PaymentFeeKobo != 16_93 {
		t.Fatalf("not stored: %+v", got)
	}
}

// The payment provider's fee is kept on the order, and a fee far above the 1.6%
// prices are built for is logged as a warning.
func TestPaymentFeeIsRecordedAndWatched(t *testing.T) {
	cases := []struct {
		name    string
		feeKobo int64 // on a ₦100 payment
		warns   bool
	}{
		{"the usual 1.61%", 1_61, false},
		{"exactly the limit", 1_70, false},
		{"a flat 100 naira appears", 100_00 / 1, true}, // would be the whole payment: absurd but must warn, not crash
		{"3%", 3_00, true},
	}
	for _, c := range cases {
		f := newFakes(t)
		k := newKoraFake(t, f)
		var logs strings.Builder
		f.svc.Log = slog.New(slog.NewTextHandler(&logs, nil))
		f.svc.FeeWarnPercent = 1.7
		ctx := context.Background()
		o, err := f.svc.Create(ctx, airtimeRequest) // ₦100
		if err != nil {
			t.Fatal(err)
		}
		k.status.Store("success")
		k.acceptedKobo.Store(o.Due())
		k.feeKobo.Store(c.feeKobo)
		if paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef); err != nil || paid == nil {
			t.Fatalf("%s: %v %v", c.name, paid, err)
		}
		got, _ := f.svc.Store.ByID(ctx, o.ID)
		if got.PaymentFeeKobo != c.feeKobo {
			t.Errorf("%s: fee stored %d, want %d", c.name, got.PaymentFeeKobo, c.feeKobo)
		}
		if warned := strings.Contains(logs.String(), "PaymentFee"); warned != c.warns {
			t.Errorf("%s: warned = %t, want %t\n%s", c.name, warned, c.warns, logs.String())
		}
	}
}

func TestFeeWatchCanBeSwitchedOff(t *testing.T) {
	f := newFakes(t)
	k := newKoraFake(t, f)
	var logs strings.Builder
	f.svc.Log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	o, _ := f.svc.Create(ctx, airtimeRequest)
	k.status.Store("success")
	k.acceptedKobo.Store(o.Due())
	k.feeKobo.Store(5_00)
	f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef)
	if strings.Contains(logs.String(), "PaymentFee") {
		t.Errorf("FeeWarnPercent 0 means no warnings: %s", logs.String())
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.PaymentFeeKobo != 5_00 {
		t.Errorf("the fee should still be recorded: %d", got.PaymentFeeKobo)
	}
}
