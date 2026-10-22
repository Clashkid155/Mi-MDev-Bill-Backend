package order

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// refundDue makes an order that is waiting for a refund, after a payment of ₦10,000
// on which the provider took ₦161.25.
func refundDue(t *testing.T, f *fakes, key string) Order {
	t.Helper()
	o := Order{IdempotencyKey: key, Kind: KindAirtime, Network: "a", Recipient: "08012345678", AmountKobo: 10_000_00, CostKobo: 9_900_00,
		TotalKobo: 10_000_00, CustomerEmail: "a@b.co", ExpiresAt: time.Now().Add(time.Hour)}
	if err := f.svc.Store.Insert(context.Background(), &o); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Store.db.Exec(`UPDATE orders SET status = 'REFUND_DUE', payment_fee_kobo = 161_25 WHERE id = ?`, o.ID); err != nil {
		t.Fatal(err)
	}
	return o
}

func postRefunded(t *testing.T, srv *httptest.Server, id, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/admin/orders/"+id+"/refunded", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply map[string]any
	json.NewDecoder(resp.Body).Decode(&reply)
	return resp.StatusCode, reply
}

func TestMarkRefunded(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	ctx := context.Background()

	o := refundDue(t, f, "r1")
	code, reply := postRefunded(t, srv, o.ID, `{"refund_fee_kobo":5000,"note":"  sent back from the Kora dashboard  "}`)
	data, _ := reply["data"].(map[string]any)
	if code != 200 || data["status"] != "REFUNDED" || data["refund_fee_kobo"] != float64(5000) ||
		data["refund_note"] != "sent back from the Kora dashboard" || data["refunded_at"] == nil {
		t.Fatalf("%d %v", code, reply)
	}
	got, _ := f.svc.Store.ByID(ctx, o.ID)
	if got.Status != StatusRefunded || got.RefundFeeKobo != 5000 || got.RefundedAt.IsZero() || got.PaymentFeeKobo != 161_25 {
		t.Fatalf("stored: %+v", got)
	}
	// the app sees REFUNDED, so it can stop saying "waiting for a refund"
	if viewOf(got).Status != StatusRefunded {
		t.Errorf("the app's view: %v", viewOf(got).Status)
	}

	// Doing it twice is refused and changes nothing.
	if code, reply := postRefunded(t, srv, o.ID, `{"refund_fee_kobo":9999}`); code != 409 || !strings.Contains(reply["error"].(string), "REFUNDED") {
		t.Errorf("second call: %d %v", code, reply)
	}
	if again, _ := f.svc.Store.ByID(ctx, o.ID); again.RefundFeeKobo != 5000 {
		t.Errorf("the first record was overwritten: %+v", again)
	}
}

func TestMarkRefundedBodyIsOptionalAndFeeCanComeBack(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	a := refundDue(t, f, "r2")
	if code, _ := postRefunded(t, srv, a.ID, ``); code != 200 {
		t.Errorf("an empty body should do: %d", code)
	}
	b := refundDue(t, f, "r3")
	if code, _ := postRefunded(t, srv, b.ID, `{"payment_fee_returned":true}`); code != 200 {
		t.Fatal(code)
	}
	if got, _ := f.svc.Store.ByID(context.Background(), b.ID); got.PaymentFeeKobo != 0 {
		t.Errorf("a fee the provider gave back is not a cost: %d", got.PaymentFeeKobo)
	}
}

func TestMarkRefundedRefusesWhatItShould(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	ctx := context.Background()

	if code, _ := postRefunded(t, srv, "MDEV_NOSUCHORDER", `{}`); code != 404 {
		t.Errorf("unknown order: %d", code)
	}
	// Only an order waiting for a refund can be closed: not a paid or delivered one.
	waiting, _ := f.svc.Create(ctx, airtimeRequest)
	if code, _ := postRefunded(t, srv, waiting.ID, `{}`); code != 409 {
		t.Errorf("an order still awaiting payment: %d", code)
	}
	delivered := f.payAndVend(t, waiting)
	if code, _ := postRefunded(t, srv, delivered.ID, `{}`); code != 409 {
		t.Errorf("a delivered order: %d", code)
	}
	if got, _ := f.svc.Store.ByID(ctx, delivered.ID); got.Status != StatusSuccess {
		t.Errorf("a delivered order must stay SUCCESS: %s", got.Status)
	}

	o := refundDue(t, f, "r4")
	for name, body := range map[string]string{
		"a negative fee":        `{"refund_fee_kobo":-1}`,
		"a fee above the order": `{"refund_fee_kobo":99999999}`,
		"a very long note":      `{"note":"` + strings.Repeat("x", 501) + `"}`,
		"broken JSON":           `{"note":`,
	} {
		if code, _ := postRefunded(t, srv, o.ID, body); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusRefundDue {
		t.Errorf("a refused call must change nothing: %s", got.Status)
	}
}

func TestRefundCostsReduceTheNetProfit(t *testing.T) {
	store := statsStore(t)
	// a refunded 10,000 order in the window: the provider kept 161.25 and sending the money back cost 50
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "MTN Airtime", AmountKobo: 10_000_00, CostKobo: 9_900_00, TotalKobo: 10_000_00,
		PaymentFeeKobo: 161_25}, StatusRefundDue, day(2026, 10, 11, 12, 0))
	var id string
	store.db.QueryRow(`SELECT id FROM orders WHERE status = 'REFUND_DUE' AND created_at = ?`, day(2026, 10, 11, 12, 0).Unix()).Scan(&id)
	if _, err := store.MarkRefunded(context.Background(), id, 50_00, 0, "", false); err != nil {
		t.Fatal(err)
	}
	st, err := store.Stats(context.Background(), day(2026, 10, 10, 0, 0), day(2026, 10, 12, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	m := st.Money
	if m.RefundFeesKobo != 50_00 || m.FeesOnUndeliveredKobo != 2*161_25 {
		t.Fatalf("refund fees %d, fees on undelivered %d", m.RefundFeesKobo, m.FeesOnUndeliveredKobo)
	}
	if m.NetProfitKobo != m.ProfitKobo-2*161_25-50_00 {
		t.Errorf("net profit %d", m.NetProfitKobo)
	}
}

// Sending back less than the customer paid (the costs of the transaction deducted)
// is recorded, shown to the customer, and counted as money kept.
func TestRefundWithTheTransactionCostsDeducted(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	ctx := context.Background()

	o := refundDue(t, f, "r5") // paid ₦10,000; the provider kept ₦161.25
	// ₦161.25 payment fee and the ₦50 refund fee deducted: ₦9,788.75 goes back
	code, reply := postRefunded(t, srv, o.ID, `{"refund_fee_kobo":5000,"refund_amount_kobo":978875,"note":"costs deducted"}`)
	data, _ := reply["data"].(map[string]any)
	if code != 200 || data["refunded_kobo"] != float64(978875) || data["status"] != "REFUNDED" {
		t.Fatalf("%d %v", code, reply)
	}
	got, _ := f.svc.Store.ByID(ctx, o.ID)
	if got.Refunded() != 978_875 || got.RefundAmountKobo != 978_875 {
		t.Errorf("stored: %+v", got)
	}
	// the app is told how much came back
	if v := viewOf(got); v.RefundedKobo != 978_875 || v.TotalKobo != 10_000_00 {
		t.Errorf("app view: %+v", v)
	}

	// Leaving it out means the whole total went back, and only a refunded order shows an amount.
	full := refundDue(t, f, "r6")
	postRefunded(t, srv, full.ID, `{}`)
	g, _ := f.svc.Store.ByID(ctx, full.ID)
	if g.Refunded() != 10_000_00 || g.RefundAmountKobo != 0 {
		t.Errorf("a full refund: refunded %d, recorded %d", g.Refunded(), g.RefundAmountKobo)
	}
	if (Order{Status: StatusRefundDue, TotalKobo: 5_00, AmountKobo: 5_00}).Refunded() != 0 {
		t.Error("an order still waiting for its refund has had nothing sent back")
	}
	body, _ := json.Marshal(viewOf(refundDue(t, f, "r7")))
	if strings.Contains(string(body), "refunded_kobo") {
		t.Errorf("not refunded yet, so no refunded_kobo: %s", body)
	}

	// More than the customer paid, or a negative amount, is refused.
	r := refundDue(t, f, "r8")
	for _, body := range []string{`{"refund_amount_kobo":10000001}`, `{"refund_amount_kobo":-5}`} {
		if code, _ := postRefunded(t, srv, r.ID, body); code != 400 {
			t.Errorf("%s: got %d, want 400", body, code)
		}
	}
	if left, _ := f.svc.Store.ByID(ctx, r.ID); left.Status != StatusRefundDue {
		t.Errorf("a refused call must change nothing: %s", left.Status)
	}
}

// The money kept out of a refund offsets what the transaction cost, so a refund
// that deducts exactly the costs leaves the net profit where it was.
func TestKeptFromRefundsOffsetsTheCosts(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/kept.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "MTN", AmountKobo: 10_000_00, CostKobo: 9_900_00, TotalKobo: 10_000_00,
		PaymentFeeKobo: 161_25}, StatusRefundDue, day(2026, 10, 16, 22, 0))
	var id string
	store.db.QueryRow(`SELECT id FROM orders`).Scan(&id)
	if _, err := store.MarkRefunded(context.Background(), id, 50_00, 10_000_00-161_25-50_00, "", false); err != nil {
		t.Fatal(err)
	}
	st, err := store.Stats(context.Background(), day(2026, 10, 16, 0, 0), day(2026, 10, 16, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	m := st.Money
	if m.KeptFromRefundsKobo != 161_25+50_00 || m.FeesOnUndeliveredKobo != 161_25 || m.RefundFeesKobo != 50_00 {
		t.Fatalf("kept %d, fees %d, refund fees %d", m.KeptFromRefundsKobo, m.FeesOnUndeliveredKobo, m.RefundFeesKobo)
	}
	if m.NetProfitKobo != 0 {
		t.Errorf("deducting exactly the costs should cost nothing, net profit %d", m.NetProfitKobo)
	}
}
