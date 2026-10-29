package order

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Bills-Backend/logging"
)

func TestRedactRemovesWhatMustNotBeKept(t *testing.T) {
	cases := map[string]string{
		"mail to buyer@example.com failed":                    "mail to [email] failed",
		"for 08012345678 on MTN":                              "for [phone] on MTN",
		"for +2348012345678":                                  "for [phone]",
		"for 2348012345678":                                   "for [phone]",
		"meter 12345678901 not found":                         "meter [number] not found",
		"token 1234-5678-9012-3456-7890 sent":                 "token [number] sent",
		"token 1234 5678 9012 3456 sent":                      "token [number] sent",
		"Authorization: Bearer abc.def-ghi_jkl123":            "Authorization: Bearer [hidden]",
		"key mdev_abcdefghijklmnopqrstuvwxyz0123456789ABCDEF": "key [token]",
		"secret 0123456789abcdef0123456789abcdef0123456789":   "secret [secret]",
		"order MDEV_9616F67242EDCFC1F3A2 is paid":             "order MDEV_9616F67242EDCFC1F3A2 is paid", // an order id stays
		"paid ₦500.00 (50000 kobo) in 1.2s":                   "paid ₦500.00 (50000 kobo) in 1.2s",       // amounts stay
		"  lots   of\n\n space  ":                             "lots of space",
		"HTTP 502 from 104.21.12.6":                           "HTTP 502 from 104.21.12.6", // an address of ours is not a customer's detail
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

// eventsFixture is a service with events switched on, and a way to read what was recorded.
func eventsFixture(t *testing.T) (*fakes, func(filter EventFilter) []Event) {
	t.Helper()
	f := newFakes(t)
	rec := NewEventRecorder(f.svc.Store, f.svc.Log)
	t.Cleanup(rec.Close)
	f.svc.Events = rec
	return f, func(filter EventFilter) []Event {
		t.Helper()
		rec.Flush()
		if filter.Limit == 0 {
			filter.Limit = 200
		}
		events, err := f.svc.Store.ListEvents(context.Background(), filter)
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

func kinds(events []Event) string {
	var k []string
	for _, e := range events {
		k = append(k, e.Kind)
	}
	return strings.Join(k, " ")
}

func TestAnOrdersLifeIsToldInEventsWithoutTheCustomersDetails(t *testing.T) {
	f, read := eventsFixture(t)
	o := f.payAndVend(t, orderWithKey(f, t, "life")) // ₦100 airtime to 08012345678 for a@b.co
	events := read(EventFilter{OrderID: o.ID})

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
	events := read(EventFilter{OrderID: o.ID})

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
	events := read(EventFilter{OrderID: o.ID})
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
	got := read(EventFilter{OrderID: expired.ID})
	if kinds(got) != "order.created order.expired payment.late" || got[2].Level != LevelWarn || !strings.Contains(got[2].Message, "refund needed") {
		t.Errorf("expired then paid: %s %+v", kinds(got), got)
	}

	f2, read2 := eventsFixture(t)
	short, _ := f2.svc.Create(ctx, airtimeRequest)
	f2.invoiceState.Store("PARTIALLY_PAID")
	f2.paidKobo.Store(50_00)
	f2.svc.ConfirmPayment(ctx, short.ID, short.TxnRef)
	f2.svc.ConfirmPayment(ctx, short.ID, short.TxnRef) // asking again must not repeat the event
	wrong := read2(EventFilter{Kind: "payment."})
	if len(wrong) != 1 || wrong[0].Kind != "payment.wrong_amount" || !strings.Contains(wrong[0].Message, "received ₦50.00, expected ₦100.00") {
		t.Errorf("a short payment: %+v", wrong)
	}

	f3, read3 := eventsFixture(t)
	f3.svc.ConfirmPayment(ctx, "MDEV_NOSUCHORDER00001", "T-1")
	unknown := read3(EventFilter{Kind: "payment.unknown"})
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
	sent := read(EventFilter{Kind: "refund.sent"})
	if len(sent) != 1 || sent[0].OrderID != o.ID || !strings.Contains(sent[0].Message, "₦100.00 back") {
		t.Errorf("refund event: %+v", sent)
	}
}

// ---- what the logger feeds in ----

func TestWarningsAndErrorsFromTheLogBecomeRedactedSystemEvents(t *testing.T) {
	f, read := eventsFixture(t)
	log := logging.New(io.Discard, slog.LevelInfo)
	logging.SetSink(log, f.svc.Events.Capture)

	log.Info("GetSystemData - ABS", "detail", "HTTP 200")                                // routine: not kept
	log.Warn("HTTP", "detail", "GET /nope -> 404 · 1ms")                                 // the access log: not kept
	log.Error("HTTP", "detail", "GET /v1/catalog -> 502 · 4s")                           // a server error: kept
	log.Warn("ConfirmPayment - Order", "detail", "order MDEV_AAAAAAAAAAAAAAAA is short") // said better by an explicit event: not kept
	log.Warn("Vend - Order", "detail", "order MDEV_AAAAAAAAAAAAAAAA: FAILED")            // same
	log.Error("Vend - Order", "detail", "order MDEV_AAAAAAAAAAAAAAAA: database is locked")
	log.Warn("BuyAirtime - ABS", "detail", "FAILED · could not deliver to 08012345678 or mail buyer@example.com")
	log.Warn("Events - Store", "detail", "could not save an event") // the recorder's own trouble: not kept

	got := read(EventFilter{Kind: "system"})
	if len(got) != 3 {
		t.Fatalf("want 3 system events, got %d: %+v", len(got), got)
	}
	byMessage := map[string]Event{}
	for _, e := range got {
		byMessage[e.Message] = e
	}
	if e := byMessage["HTTP: GET /v1/catalog -> 502 · 4s"]; e.Level != LevelError {
		t.Errorf("a 5xx should be an error event: %+v", got)
	}
	if e := byMessage["Vend - Order: order MDEV_AAAAAAAAAAAAAAAA: database is locked"]; e.Level != LevelError || e.OrderID != "MDEV_AAAAAAAAAAAAAAAA" {
		t.Errorf("a database error should keep its order id: %+v", got)
	}
	redacted := byMessage["BuyAirtime - ABS: FAILED · could not deliver to [phone] or mail [email]"]
	if redacted.Level != LevelWarn {
		t.Errorf("customers' details must be removed: %+v", got)
	}
}

func TestIdenticalWarningsAreKeptOnceAndAFloodIsCapped(t *testing.T) {
	f, read := eventsFixture(t)
	for i := 0; i < 50; i++ {
		f.svc.Events.Capture(logging.Entry{Time: time.Now(), Level: slog.LevelWarn, Tag: "GetSystemData - ABS", Detail: "HTTP 503"})
	}
	if n := len(read(EventFilter{Kind: "system"})); n != 1 {
		t.Errorf("50 identical warnings made %d events, want 1", n)
	}
	for i := 0; i < 300; i++ {
		f.svc.Events.Capture(logging.Entry{Time: time.Now(), Level: slog.LevelWarn, Tag: "Other - ABS", Detail: fmt.Sprintf("different problem %d", i)})
	}
	if n := len(read(EventFilter{Kind: "system"})); n > capturesPerMinut+1 {
		t.Errorf("a flood of different warnings made %d events; the cap is %d a minute", n, capturesPerMinut)
	}
}

func TestRecordingNeverBlocksOrPanicsAfterClose(t *testing.T) {
	f := newFakes(t)
	rec := NewEventRecorder(f.svc.Store, f.svc.Log)
	rec.Record(LevelInfo, "x", "", "before", nil)
	rec.Close()
	done := make(chan struct{})
	go func() {
		rec.Record(LevelInfo, "x", "", "after the close", nil)
		rec.Capture(logging.Entry{Level: slog.LevelError, Tag: "T", Detail: "late"})
		rec.Close() // a second close is harmless
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("recording after Close blocked")
	}
	var nilRec *EventRecorder
	nilRec.Record(LevelInfo, "x", "", "events switched off", nil) // a nil recorder is a no-op
	nilRec.Flush()
	nilRec.Close()
	events, _ := f.svc.Store.ListEvents(context.Background(), EventFilter{Limit: 10})
	if len(events) != 1 || events[0].Message != "before" {
		t.Errorf("what was queued before Close must be written, nothing after: %+v", events)
	}
}

func TestManyEventsAtOnceAreAllWritten(t *testing.T) {
	f, read := eventsFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.svc.Events.Record(LevelInfo, "test.many", "", fmt.Sprint("event ", i), nil)
		}(i)
	}
	wg.Wait()
	if n := len(read(EventFilter{Kind: "test.many"})); n != 200 {
		t.Errorf("%d of 200 events were written", n)
	}
}

func TestTheRecorderWorksFromTheLoggerEndToEnd(t *testing.T) {
	f, read := eventsFixture(t)
	var printed bytes.Buffer
	log := logging.New(&printed, slog.LevelInfo)
	logging.SetSink(log, f.svc.Events.Capture)
	log.Warn("GetSystemData - ABS", "detail", "HTTP 401 · unauthorised for buyer@example.com")
	got := read(EventFilter{MinLevel: LevelWarn})
	if len(got) != 1 || strings.Contains(got[0].Message, "buyer@example.com") || !strings.Contains(printed.String(), "buyer@example.com") {
		t.Errorf("the log keeps what happened, the event keeps it without the customer: event %+v, log %q", got, printed.String())
	}
}

// ---- retention ----

func TestOldEventsArePurgedAndRetentionZeroKeepsAll(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	old := time.Now().AddDate(0, 0, -200).Unix()
	f.svc.Store.AddEvent(ctx, Event{At: old, Level: LevelInfo, Kind: "old", Message: "old"})
	f.svc.Store.AddEvent(ctx, Event{Level: LevelInfo, Kind: "new", Message: "new"})

	(&API{Service: f.svc}).purgeEvents(ctx) // retention 0: keep everything
	if got, _ := f.svc.Store.ListEvents(ctx, EventFilter{Limit: 10}); len(got) != 2 {
		t.Fatalf("retention 0 must delete nothing: %d left", len(got))
	}
	api := &API{Service: f.svc, EventRetention: 180 * 24 * time.Hour}
	api.purgeEvents(ctx)
	got, _ := f.svc.Store.ListEvents(ctx, EventFilter{Limit: 10})
	if len(got) != 1 || got[0].Kind != "new" {
		t.Errorf("after the purge: %+v", got)
	}
}

// ---- the API ----

func TestTheEventsEndpoints(t *testing.T) {
	srv, f, _ := adminServer(t, ownerToken)
	ctx := context.Background()
	add := func(at time.Time, level, kind, order, msg string) {
		f.svc.Store.AddEvent(ctx, Event{At: at.Unix(), Level: level, Kind: kind, OrderID: order, Message: msg, Detail: json.RawMessage(`{"n":1}`)})
	}
	now := time.Now()
	add(now.Add(-48*time.Hour), LevelInfo, "order.created", "MDEV_A", "two days ago")
	add(now.Add(-2*time.Hour), LevelInfo, "order.paid", "MDEV_A", "paid")
	add(now.Add(-time.Hour), LevelWarn, "payment.late", "MDEV_B", "late")
	add(now.Add(-30*time.Minute), LevelError, "order.failed", "MDEV_B", "failed")
	add(now.Add(-time.Minute), LevelError, "system", "", "provider down")

	list := func(query string) []any {
		code, data := call(t, srv, "GET", "/admin/events"+query, ownerToken, nil)
		if code != 200 {
			t.Fatalf("%s answered %d", query, code)
		}
		return data["events"].([]any)
	}
	if got := list(""); len(got) != 5 || got[0].(map[string]any)["message"] != "provider down" {
		t.Errorf("newest first: %v", got)
	}
	if got := list("?level=warn"); len(got) != 3 {
		t.Errorf("level=warn is warnings and errors: %d", len(got))
	}
	if got := list("?level=error"); len(got) != 2 {
		t.Errorf("level=error: %d", len(got))
	}
	if got := list("?kind=order."); len(got) != 3 {
		t.Errorf("kind=order. is the family: %d", len(got))
	}
	if got := list("?kind=payment.late"); len(got) != 1 {
		t.Errorf("an exact kind: %d", len(got))
	}
	if got := list("?order=MDEV_B"); len(got) != 2 {
		t.Errorf("one order's events: %d", len(got))
	}
	if got := list("?since=" + now.Add(-3*time.Hour).UTC().Format(time.RFC3339)); len(got) != 4 {
		t.Errorf("since a time: %d", len(got))
	}
	if got := list("?since=" + now.AddDate(0, 0, -30).In(lagos).Format("2006-01-02")); len(got) != 5 {
		t.Errorf("since a day: %d", len(got))
	}
	first := list("?limit=2")
	lastID := int64(first[1].(map[string]any)["id"].(float64))
	if next := list(fmt.Sprintf("?limit=2&before_id=%d", lastID)); len(first) != 2 || len(next) != 2 {
		t.Errorf("paging: %d then %d", len(first), len(next))
	}
	if d := list("?order=MDEV_A")[0].(map[string]any)["detail"].(map[string]any); d["n"] != 1.0 {
		t.Errorf("detail should come back as an object: %v", d)
	}
	for _, bad := range []string{"?level=loud", "?limit=0", "?limit=999", "?before_id=x", "?since=yesterday"} {
		if code, _ := call(t, srv, "GET", "/admin/events"+bad, ownerToken, nil); code != 400 {
			t.Errorf("%s answered %d, want 400", bad, code)
		}
	}

	code, sum := call(t, srv, "GET", "/admin/events/summary", ownerToken, nil)
	latest, _ := sum["latest_error"].(map[string]any)
	if code != 200 || sum["info"] != 1.0 || sum["warn"] != 1.0 || sum["error"] != 2.0 || latest["message"] != "provider down" {
		t.Errorf("summary for 24 hours: %d %v", code, sum)
	}
	if _, wide := call(t, srv, "GET", "/admin/events/summary?hours=720", ownerToken, nil); wide["info"] != 2.0 {
		t.Errorf("summary for 720 hours: %v", wide)
	}
	for _, bad := range []string{"?hours=0", "?hours=721", "?hours=x"} {
		if code, _ := call(t, srv, "GET", "/admin/events/summary"+bad, ownerToken, nil); code != 400 {
			t.Errorf("summary%s answered %d", bad, code)
		}
	}

	// Who may read them.
	viewer, _ := makeToken(t, srv, "dashboard", RoleViewer)
	admin, _ := makeToken(t, srv, "phone", RoleAdmin)
	for _, path := range []string{"/admin/events", "/admin/events/summary"} {
		if code, _ := call(t, srv, "GET", path, viewer, nil); code != 403 {
			t.Errorf("a viewer read %s: %d", path, code)
		}
		if code, _ := call(t, srv, "GET", path, admin, nil); code != 200 {
			t.Errorf("an admin reading %s: %d", path, code)
		}
	}
}
