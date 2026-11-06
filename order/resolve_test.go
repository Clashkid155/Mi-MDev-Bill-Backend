package order

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Bills-Backend/platform"
)

// pendingOrder is an order ABS never answered: the customer paid, the purchase call timed out.
func pendingOrder(t *testing.T, f *fakes, key string) Order {
	t.Helper()
	f.absFails(`gateway timeout`, 504)
	o := f.payAndVend(t, orderWithKey(f, t, key))
	if o.Status != StatusPending {
		t.Fatalf("status %s, want PENDING", o.Status)
	}
	return o
}

func resolve(t *testing.T, srv *httptest.Server, id, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/admin/orders/"+id+"/resolve", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// receiptsTo counts the emails the customer was sent.
func (f *fakes) receiptsTo(addr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.allEmails {
		if m.To == addr {
			n++
		}
	}
	return n
}

func TestAPendingOrderMarkedDeliveredBecomesSuccessAndTheCustomerGetsTheReceipt(t *testing.T) {
	f, read := eventsFixture(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := pendingOrder(t, f, "res-ok")
	if f.receiptsTo("a@b.co") != 0 {
		t.Fatal("no receipt may go out while the outcome is unknown")
	}

	code, out := resolve(t, srv, o.ID, `{"outcome":"Delivered","note":"  ABS shows it settled, data arrived  "}`)
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["status"] != "SUCCESS" {
		t.Fatalf("%d %v", code, out)
	}
	if got, _ := f.svc.Store.ByID(context.Background(), o.ID); got.Status != StatusSuccess {
		t.Errorf("stored status %s", got.Status)
	}
	if f.receiptsTo("a@b.co") != 1 {
		t.Errorf("the customer got %d receipts, want 1", f.receiptsTo("a@b.co"))
	}
	if f.spendCalls.Load() != 1 {
		t.Errorf("ABS was called %d times: resolving must never buy again", f.spendCalls.Load())
	}
	events := read(platform.EventFilter{OrderID: o.ID, Kind: "order.resolved"})
	if len(events) != 1 || events[0].Level != LevelInfo || !strings.Contains(events[0].Message, "marked delivered by unknown") ||
		!strings.Contains(events[0].Message, "ABS shows it settled") {
		t.Errorf("events %+v", events)
	}
}

func TestAPendingOrderMarkedFailedBecomesARefundDue(t *testing.T) {
	f, read := eventsFixture(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := pendingOrder(t, f, "res-fail")

	code, out := resolve(t, srv, o.ID, `{"outcome":"failed"}`)
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["status"] != "REFUND_DUE" {
		t.Fatalf("%d %v", code, out)
	}
	if f.receiptsTo("a@b.co") != 0 || f.spendCalls.Load() != 1 {
		t.Error("a failed order sends no receipt and never buys again")
	}
	events := read(platform.EventFilter{OrderID: o.ID, Kind: "order.resolved"})
	if len(events) != 1 || events[0].Level != LevelWarn || !strings.Contains(events[0].Message, "refund due") {
		t.Errorf("events %+v", events)
	}

	// And the usual refund steps carry on from there.
	if code, _ := postRefunded(t, srv, o.ID, `{"note":"sent back"}`); code != 200 {
		t.Errorf("recording the refund answered %d", code)
	}
}

func TestOnlyAPendingOrderCanBeResolvedAndOnlyOnce(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()

	o := pendingOrder(t, f, "res-twice")
	if code, _ := resolve(t, srv, o.ID, `{"outcome":"delivered"}`); code != 200 {
		t.Fatalf("first answer %d", code)
	}
	// A second click, or the opposite decision later, changes nothing.
	for _, body := range []string{`{"outcome":"delivered"}`, `{"outcome":"failed"}`} {
		code, out := resolve(t, srv, o.ID, body)
		if code != 409 || !strings.Contains(out["error"].(string), "this one is SUCCESS") {
			t.Errorf("%s again: %d %v", body, code, out)
		}
	}
	if got, _ := f.svc.Store.ByID(context.Background(), o.ID); got.Status != StatusSuccess || f.receiptsTo("a@b.co") != 1 {
		t.Errorf("status %s, %d receipts", got.Status, f.receiptsTo("a@b.co"))
	}

	// Orders in other states are refused: a waiting one, a refund due one.
	f.absFails(`{"status":true,"text":"Successful","data":{}}`, 200)
	waiting := orderWithKey(f, t, "res-waiting")
	if code, out := resolve(t, srv, waiting.ID, `{"outcome":"delivered"}`); code != 409 || !strings.Contains(out["error"].(string), "AWAITING_PAYMENT") {
		t.Errorf("a waiting order: %d %v", code, out)
	}
	due := refundDue(t, f, "res-due")
	if code, _ := resolve(t, srv, due.ID, `{"outcome":"delivered"}`); code != 409 {
		t.Errorf("a refund due order: %d, want 409", code)
	}
	if got, _ := f.svc.Store.ByID(context.Background(), due.ID); got.Status != StatusRefundDue {
		t.Errorf("a refused resolve changed the order to %s", got.Status)
	}
}

func TestResolveRefusesBadRequests(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := pendingOrder(t, f, "res-bad")

	for name, body := range map[string]string{
		"no body": ``, "not json": `nope`, "no outcome": `{}`, "unknown outcome": `{"outcome":"refunded"}`,
		"long note": `{"outcome":"failed","note":"` + strings.Repeat("a", 501) + `"}`,
	} {
		if code, _ := resolve(t, srv, o.ID, body); code != 400 {
			t.Errorf("%s answered %d, want 400", name, code)
		}
	}
	if code, _ := resolve(t, srv, "MDEV_nothere", `{"outcome":"delivered"}`); code != 404 {
		t.Errorf("an unknown order answered %d, want 404", code)
	}
	if got, _ := f.svc.Store.ByID(context.Background(), o.ID); got.Status != StatusPending {
		t.Errorf("refused requests changed the order to %s", got.Status)
	}
}

// Electricity: the token is only in ABS's reply, which never came, so no receipt can be made.
func TestAnElectricityOrderMarkedDeliveredGetsNoReceipt(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := Order{IdempotencyKey: "res-power", Kind: KindElectricity, Network: "d", Recipient: "12345678901", AmountKobo: 2_000_00, CostKobo: 2_000_00,
		TotalKobo: 2_000_00, CustomerEmail: "a@b.co", ExpiresAt: time.Now().Add(time.Hour)}
	if err := f.svc.Store.Insert(context.Background(), &o); err != nil {
		t.Fatal(err)
	}
	f.svc.Store.db.Exec(`UPDATE orders SET status = 'PENDING' WHERE id = ?`, o.ID)

	if code, _ := resolve(t, srv, o.ID, `{"outcome":"delivered"}`); code != 200 {
		t.Fatalf("answered %d", code)
	}
	if got, _ := f.svc.Store.ByID(context.Background(), o.ID); got.Status != StatusSuccess {
		t.Errorf("status %s", got.Status)
	}
	if f.receiptsTo("a@b.co") != 0 {
		t.Error("a receipt without the token would mislead the customer: none must be sent")
	}
}

// Two people deciding at once (or a double tap): exactly one decision wins and the order ends in a
// state that matches it.
func TestTwoDecisionsAtOnceLeaveOneWinner(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := pendingOrder(t, f, "res-race")

	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		body := `{"outcome":"delivered"}`
		if i%2 == 1 {
			body = `{"outcome":"failed"}`
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := resolve(t, srv, o.ID, body)
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	won := 0
	for c := range codes {
		switch c {
		case 200:
			won++
		case 409:
		default:
			t.Errorf("unexpected answer %d", c)
		}
	}
	got, _ := f.svc.Store.ByID(context.Background(), o.ID)
	if won != 1 || (got.Status != StatusSuccess && got.Status != StatusRefundDue) {
		t.Errorf("%d winners, final status %s", won, got.Status)
	}
	if got.Status == StatusSuccess && f.receiptsTo("a@b.co") != 1 || got.Status == StatusRefundDue && f.receiptsTo("a@b.co") != 0 {
		t.Errorf("status %s but %d receipts", got.Status, f.receiptsTo("a@b.co"))
	}
}

func TestTheUnknownOutcomeAlertSaysHowToCloseTheOrder(t *testing.T) {
	f := newFakes(t)
	alerts, sent := alertsOn(t, f)
	f.absFails(`gateway timeout`, 504)
	o := f.payAndVend(t, orderWithKey(f, t, "res-mail"))
	if subjects := sent(); len(subjects) != 1 || !strings.Contains(subjects[0], o.ID) {
		t.Fatalf("%q", subjects)
	}
	_ = alerts
	f.mu.Lock()
	defer f.mu.Unlock()
	var body string
	for _, m := range f.allEmails {
		if strings.Contains(m.Subject, "Outcome unknown") {
			body = m.Text
		}
	}
	for _, want := range []string{"/admin/orders/{id}/resolve", `"delivered"`, `"failed"`, "never buys it again"} {
		if !strings.Contains(body, want) {
			t.Errorf("the alert does not say %q:\n%s", want, body)
		}
	}
}
