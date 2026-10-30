package order

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"Bills-Backend/platform"
)

// eventsFixture is a service with events switched on, and a way to read what was recorded.
func eventsFixture(t *testing.T) (*fakes, func(filter platform.EventFilter) []platform.Event) {
	t.Helper()
	f := newFakes(t)
	shared, err := platform.NewStore(f.svc.Store.DB())
	if err != nil {
		t.Fatal(err)
	}
	rec := platform.NewEventRecorder(shared, f.svc.Log)
	t.Cleanup(rec.Close)
	f.svc.Events = rec
	return f, func(filter platform.EventFilter) []platform.Event {
		t.Helper()
		rec.Flush()
		if filter.Limit == 0 {
			filter.Limit = 200
		}
		events, err := shared.ListEvents(context.Background(), filter)
		if err != nil {
			t.Fatal(err)
		}
		// Oldest first, so a test reads like the order's life.
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
		return events
	}
}

func kinds(events []platform.Event) string {
	var k []string
	for _, e := range events {
		k = append(k, e.Kind)
	}
	return strings.Join(k, " ")
}

func TestAnOrdersLifeIsToldInEventsWithoutTheCustomersDetails(t *testing.T) {
	f, read := eventsFixture(t)
	o := f.payAndVend(t, orderWithKey(f, t, "life")) // ₦100 airtime to 08012345678 for a@b.co
	events := read(platform.EventFilter{OrderID: o.ID})

	if got := kinds(events); got != "order.created order.paid order.delivered" {
		t.Fatalf("an order's life: %s", got)
	}
	for _, e := range events {
		if e.Level != LevelInfo || e.OrderID != o.ID || e.At < 1e9 || e.Message == "" {
			t.Errorf("event %+v", e)
		}
		blob := e.Message + string(e.Detail)
		for _, secret := range []string{"08012345678", "a@b.co", "12345678"} {
			if strings.Contains(blob, secret) {
				t.Errorf("%s kept a customer's detail %q: %s", e.Kind, secret, blob)
			}
		}
	}
	if !strings.Contains(events[0].Message, "airtime") || !strings.Contains(events[0].Message, "₦100.00") {
		t.Errorf("created: %q", events[0].Message)
	}
	var detail map[string]any
	json.Unmarshal(events[1].Detail, &detail)
	if detail["received_kobo"] != 10000.0 {
		t.Errorf("paid detail: %s", events[1].Detail)
	}
}

func TestAFailedOrderIsAnErrorEventWithTheReason(t *testing.T) {
	f, read := eventsFixture(t)
	f.absFails(insufficient, 400)
	o := f.payAndVend(t, orderWithKey(f, t, "fail"))
	events := read(platform.EventFilter{OrderID: o.ID})

	if got := kinds(events); got != "order.created order.paid order.failed" {
		t.Fatalf("kinds: %s", got)
	}
	failed := events[2]
	if failed.Level != LevelError || !strings.Contains(failed.Message, "Insufficient balance") || !strings.Contains(failed.Message, "refund due ₦100.00") {
		t.Errorf("failed: %+v", failed)
	}
}

func TestAnUnknownOutcomeIsAWarning(t *testing.T) {
	f, read := eventsFixture(t)
	f.absFails(`gateway timeout`, 504) // not a reply ABS would give: the outcome is unknown
	o := f.payAndVend(t, orderWithKey(f, t, "pending"))
	events := read(platform.EventFilter{OrderID: o.ID})
	last := events[len(events)-1]
	if last.Kind != "order.pending" || last.Level != LevelWarn {
		t.Errorf("an unknown outcome should be a warning: %s", kinds(events))
	}
}

func TestExpiryLatePaymentWrongAmountAndUnknownPayment(t *testing.T) {
	f, read := eventsFixture(t)
	ctx := context.Background()

	expired, _ := f.svc.Create(ctx, airtimeRequest)
	f.svc.Settle(ctx, f.expire(t, expired))
	f.invoiceState.Store("PAID")
	f.svc.ConfirmPayment(ctx, expired.ID, expired.TxnRef) // paid after it expired
	got := read(platform.EventFilter{OrderID: expired.ID})
	if kinds(got) != "order.created order.expired payment.late" || got[2].Level != LevelWarn || !strings.Contains(got[2].Message, "refund needed") {
		t.Errorf("expired then paid: %s %+v", kinds(got), got)
	}

	f2, read2 := eventsFixture(t)
	short, _ := f2.svc.Create(ctx, airtimeRequest)
	f2.invoiceState.Store("PARTIALLY_PAID")
	f2.paidKobo.Store(50_00)
	f2.svc.ConfirmPayment(ctx, short.ID, short.TxnRef)
	f2.svc.ConfirmPayment(ctx, short.ID, short.TxnRef) // asking again must not repeat the event
	wrong := read2(platform.EventFilter{Kind: "payment."})
	if len(wrong) != 1 || wrong[0].Kind != "payment.wrong_amount" || !strings.Contains(wrong[0].Message, "received ₦50.00, expected ₦100.00") {
		t.Errorf("a short payment: %+v", wrong)
	}

	f3, read3 := eventsFixture(t)
	f3.svc.ConfirmPayment(ctx, "MDEV_NOSUCHORDER00001", "T-1")
	unknown := read3(platform.EventFilter{Kind: "payment.unknown"})
	if len(unknown) != 1 || unknown[0].OrderID != "" || unknown[0].Level != LevelWarn {
		t.Errorf("a payment for an order we do not know: %+v", unknown)
	}
}

func TestMarkingARefundSentIsAnEvent(t *testing.T) {
	f, read := eventsFixture(t)
	f.absFails(insufficient, 400)
	o := f.payAndVend(t, orderWithKey(f, t, "refund"))
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	code, _ := postRefunded(t, srv, o.ID, `{"refund_fee_kobo":5000,"refund_amount_kobo":10000,"note":"sent"}`)
	if code != 200 {
		t.Fatalf("refund answered %d", code)
	}
	sent := read(platform.EventFilter{Kind: "refund.sent"})
	if len(sent) != 1 || sent[0].OrderID != o.ID || !strings.Contains(sent[0].Message, "₦100.00 back") {
		t.Errorf("refund event: %+v", sent)
	}
}

// ---- what the logger feeds in ----
