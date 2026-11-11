package order

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/platform"
)

const absSuccess = `{"status":true,"text":"Successful","data":{}}`

// setCost fixes what ABS charged us for an order, which is what its webhook reports.
func setCost(t *testing.T, f *fakes, id string, kobo int64) {
	t.Helper()
	if _, err := f.svc.Store.db.Exec(`UPDATE orders SET cost_kobo = ? WHERE id = ?`, kobo, id); err != nil {
		t.Fatal(err)
	}
}

// vendedOrder takes an order for recipient to the state ABS's answer leaves it in. Its cost is 99 naira.
func vendedOrder(t *testing.T, f *fakes, key, recipient, absBody string, absStatus int32) Order {
	t.Helper()
	req := airtimeRequest
	req.IdempotencyKey, req.Recipient = key, recipient
	o, err := f.svc.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	setCost(t, f, o.ID, 99_00)
	f.absFails(absBody, absStatus)
	return f.payAndVend(t, o)
}

func pendingFor(t *testing.T, f *fakes, key, recipient string) Order {
	t.Helper()
	o := vendedOrder(t, f, key, recipient, `gateway timeout`, 504)
	if o.Status != StatusPending {
		t.Fatalf("status %s, want PENDING", o.Status)
	}
	return o
}

func settledFor(t *testing.T, f *fakes, key, recipient string) Order {
	t.Helper()
	o := vendedOrder(t, f, key, recipient, absSuccess, 200)
	if o.Status != StatusSuccess {
		t.Fatalf("status %s, want SUCCESS", o.Status)
	}
	return o
}

func settledHook(ref, recipient, amount string) abs.Event {
	return abs.Event{Name: "transaction.settled", Status: "1", OrderID: ref, Recipient: recipient, Amount: amount,
		Message: "Bought 100 NGN Airtel Airtime Nigeria for " + recipient + " and paid A" + amount}
}

func failedHook(ref, recipient, amount string) abs.Event {
	return abs.Event{Name: "transaction.failed", Status: "3", OrderID: ref, Recipient: recipient, Amount: amount, Message: "Provider could not credit the line"}
}

func orderNow(t *testing.T, f *fakes, id string) Order {
	t.Helper()
	o, err := f.svc.Store.ByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func eventsOf(read func(platform.EventFilter) []platform.Event, kind string) []platform.Event {
	return read(platform.EventFilter{Kind: kind})
}

func TestASettledWebhookSettlesTheOnePendingOrderItFits(t *testing.T) {
	f, read := eventsFixture(t)
	o := pendingFor(t, f, "hook-ok", "09073931630")
	if f.receiptsTo("a@b.co") != 0 {
		t.Fatal("no receipt may go out while the outcome is unknown")
	}

	// ABS sends the amount as text, in naira.
	if err := f.svc.SettleFromABS(context.Background(), settledHook("BA202611061957DSO1937JJFK254070W", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	got := orderNow(t, f, o.ID)
	if got.Status != StatusSuccess || got.VendorRef != "BA202611061957DSO1937JJFK254070W" {
		t.Errorf("status %s, vendor ref %q", got.Status, got.VendorRef)
	}
	if f.receiptsTo("a@b.co") != 1 {
		t.Errorf("the customer got %d receipts, want 1", f.receiptsTo("a@b.co"))
	}
	if f.spendCalls.Load() != 1 {
		t.Errorf("ABS was called %d times: a webhook must never buy again", f.spendCalls.Load())
	}
	ev := eventsOf(read, "order.resolved")
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "marked delivered by ABS webhook") {
		t.Errorf("events %+v", ev)
	}
}

func TestAFailedWebhookMakesAPendingOrderARefundDueAndTellsTheOwner(t *testing.T) {
	f, _ := eventsFixture(t)
	_, sent := alertsOn(t, f)
	o := pendingFor(t, f, "hook-fail", "09073931630")
	before := len(sent())

	if err := f.svc.SettleFromABS(context.Background(), failedHook("BD1", "09073931630", "99.00")); err != nil {
		t.Fatal(err)
	}
	got := orderNow(t, f, o.ID)
	if got.Status != StatusRefundDue || got.VendorRef != "BD1" {
		t.Errorf("status %s, vendor ref %q", got.Status, got.VendorRef)
	}
	if f.receiptsTo("a@b.co") != 0 {
		t.Error("a failed purchase sends no receipt")
	}
	subjects := sent()
	if len(subjects) != before+1 || !strings.Contains(subjects[len(subjects)-1], "Refund due") {
		t.Errorf("owner emails %q", subjects)
	}
}

func TestADeliveryArrivingTwiceChangesNothingTheSecondTime(t *testing.T) {
	f, read := eventsFixture(t)
	o := pendingFor(t, f, "hook-twice", "09073931630")
	hook := settledHook("BA1", "09073931630", "99")
	for i := 0; i < 3; i++ {
		if err := f.svc.SettleFromABS(context.Background(), hook); err != nil {
			t.Fatal(err)
		}
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusSuccess || f.receiptsTo("a@b.co") != 1 {
		t.Errorf("status %s, %d receipts", got.Status, f.receiptsTo("a@b.co"))
	}
	if n := len(eventsOf(read, "order.resolved")); n != 1 {
		t.Errorf("%d resolved events, want 1", n)
	}
	if n := len(eventsOf(read, "abs.conflict")) + len(eventsOf(read, "abs.ambiguous")); n != 0 {
		t.Errorf("a repeat delivery raised %d warnings", n)
	}
}

func TestAWebhookThatFitsSeveralOrdersChangesNone(t *testing.T) {
	f, read := eventsFixture(t)
	a := pendingFor(t, f, "hook-a", "09073931630")
	b := pendingFor(t, f, "hook-b", "09073931630")

	if err := f.svc.SettleFromABS(context.Background(), settledHook("BA1", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got := orderNow(t, f, id); got.Status != StatusPending || got.VendorRef != "" {
			t.Errorf("%s: status %s, vendor ref %q: an ambiguous webhook must leave it alone", id, got.Status, got.VendorRef)
		}
	}
	ev := eventsOf(read, "abs.ambiguous")
	if len(ev) != 1 || ev[0].Level != LevelWarn || !strings.Contains(ev[0].Message, a.ID) || !strings.Contains(ev[0].Message, b.ID) ||
		!strings.Contains(string(ev[0].Detail), `"abs_order":"BA1"`) {
		t.Errorf("events %+v", ev)
	}
}

// The case that would go wrong with a looser rule: one purchase succeeded, an identical one is unknown.
// The first one's webhook must not settle the second.
func TestASuccessfulTwinOfAPendingOrderMakesTheWebhookAmbiguous(t *testing.T) {
	f, read := eventsFixture(t)
	done := settledFor(t, f, "twin-done", "09073931630")
	stuck := pendingFor(t, f, "twin-stuck", "09073931630")

	if err := f.svc.SettleFromABS(context.Background(), failedHook("BD9", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, stuck.ID); got.Status != StatusPending {
		t.Errorf("the pending order became %s on a webhook that may be about the other one", got.Status)
	}
	if got := orderNow(t, f, done.ID); got.Status != StatusSuccess {
		t.Errorf("the delivered order became %s", got.Status)
	}
	if len(eventsOf(read, "abs.ambiguous")) != 1 {
		t.Error("the owner should be told it could not be matched")
	}
}

// Once the first purchase's own webhook has been matched, its twin is no longer in doubt.
func TestAClaimedOrderStopsBeingACandidateForLaterWebhooks(t *testing.T) {
	f, _ := eventsFixture(t)
	done := settledFor(t, f, "claim-done", "09073931630")
	if err := f.svc.SettleFromABS(context.Background(), settledHook("BA1", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, done.ID); got.Status != StatusSuccess || got.VendorRef != "BA1" {
		t.Fatalf("a webhook for a delivered order should only note ABS's id: status %s, ref %q", got.Status, got.VendorRef)
	}
	if f.receiptsTo("a@b.co") != 1 {
		t.Errorf("%d receipts: a delivered order must not be mailed twice", f.receiptsTo("a@b.co"))
	}

	stuck := pendingFor(t, f, "claim-stuck", "09073931630")
	if err := f.svc.SettleFromABS(context.Background(), settledHook("BA2", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, stuck.ID); got.Status != StatusSuccess || got.VendorRef != "BA2" {
		t.Errorf("status %s, ref %q", got.Status, got.VendorRef)
	}
	if got := orderNow(t, f, done.ID); got.VendorRef != "BA1" {
		t.Errorf("the first order's ABS id became %q", got.VendorRef)
	}
}

func TestAWebhookThatFitsNoOrderIsRecordedAndChangesNothing(t *testing.T) {
	f, read := eventsFixture(t)
	o := pendingFor(t, f, "hook-none", "09073931630")
	ctx := context.Background()

	for name, ev := range map[string]abs.Event{
		"another number": settledHook("BA1", "08099999999", "99"),
		"another amount": settledHook("BA2", "09073931630", "100"),
	} {
		if err := f.svc.SettleFromABS(ctx, ev); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusPending || got.VendorRef != "" {
		t.Errorf("status %s, ref %q", got.Status, got.VendorRef)
	}
	if ev := eventsOf(read, "abs.unmatched"); len(ev) != 2 || ev[0].Level != LevelInfo {
		t.Errorf("events %+v", ev)
	}

	// An order made long ago is not a candidate.
	f.svc.Store.db.Exec(`UPDATE orders SET created_at = ? WHERE id = ?`, time.Now().Add(-vendorMatchWindow-time.Hour).Unix(), o.ID)
	if err := f.svc.SettleFromABS(ctx, settledHook("BA3", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusPending {
		t.Errorf("an order from days ago was settled by a webhook: %s", got.Status)
	}
}

func TestOrdersThatNeverReachedABSAreNotCandidates(t *testing.T) {
	f, _ := eventsFixture(t)
	waiting := orderWithKey(f, t, "hook-waiting")
	setCost(t, f, waiting.ID, 99_00)
	if err := f.svc.SettleFromABS(context.Background(), settledHook("BA1", waiting.Recipient, "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, waiting.ID); got.Status != StatusAwaitingPayment || got.VendorRef != "" {
		t.Errorf("an unpaid order was touched: %s, %q", got.Status, got.VendorRef)
	}
}

// ABS and our records disagree: do nothing to the order, but make sure the owner knows.
func TestAWebhookThatContradictsTheOrderIsOnlyReported(t *testing.T) {
	f, read := eventsFixture(t)
	_, sent := alertsOn(t, f)
	ctx := context.Background()

	delivered := settledFor(t, f, "conf-ok", "09073931630")
	if err := f.svc.SettleFromABS(ctx, failedHook("BD1", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, delivered.ID); got.Status != StatusSuccess {
		t.Errorf("a delivered order became %s", got.Status)
	}

	refused := vendedOrder(t, f, "conf-refused", "08055555555", insufficient, 400)
	if refused.Status != StatusRefundDue {
		t.Fatalf("status %s", refused.Status)
	}
	if err := f.svc.SettleFromABS(ctx, settledHook("BA2", "08055555555", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, refused.ID); got.Status != StatusRefundDue {
		t.Errorf("a refund due order became %s", got.Status)
	}

	ev := eventsOf(read, "abs.conflict")
	if len(ev) != 2 || ev[0].Level != LevelError || ev[1].Level != LevelError {
		t.Fatalf("events %+v", ev)
	}
	var found bool
	for _, s := range sent() {
		found = found || strings.Contains(s, "ABS delivered an order recorded as REFUND_DUE")
	}
	if !found {
		t.Errorf("the owner was not told a refund may be wrong: %q", sent())
	}
}

func TestAnOrderBeingBoughtNowAsksABSToDeliverAgain(t *testing.T) {
	f, _ := eventsFixture(t)
	o := orderWithKey(f, t, "hook-vending")
	setCost(t, f, o.ID, 99_00)
	f.svc.Store.db.Exec(`UPDATE orders SET status = 'VENDING' WHERE id = ?`, o.ID)

	err := f.svc.SettleFromABS(context.Background(), settledHook("BA1", o.Recipient, "99"))
	if !errors.Is(err, ErrStillVending) {
		t.Fatalf("got %v, want ErrStillVending", err)
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusVending || got.VendorRef != "" {
		t.Errorf("status %s, ref %q: the purchase in progress must be left to finish", got.Status, got.VendorRef)
	}
}

func TestOtherABSMessagesAreIgnored(t *testing.T) {
	f, read := eventsFixture(t)
	o := pendingFor(t, f, "hook-other", "09073931630")
	for _, ev := range []abs.Event{
		{Name: "wallet.credited", Status: "1", OrderID: "BA1", Recipient: "09073931630", Amount: "99"},
		{Name: "refund.issued", Status: "6", OrderID: "BA2", Recipient: "09073931630", Amount: "99"},
		{Name: "transaction.settled", Status: "3", OrderID: "BA3", Recipient: "09073931630", Amount: "99"}, // name and status disagree
		{Name: "transaction.settled", Status: "1", OrderID: "BA4", Recipient: "", Amount: "99"},
		{Name: "transaction.settled", Status: "1", OrderID: "BA5", Recipient: "09073931630", Amount: "lots"},
		{Name: "transaction.settled", Status: "1", OrderID: "BA6", Recipient: "09073931630", Amount: "-99"},
		{Name: "transaction.settled", Status: "1", OrderID: "", Recipient: "09073931630", Amount: "99"},
	} {
		if err := f.svc.SettleFromABS(context.Background(), ev); err != nil {
			t.Errorf("%s %s: %v", ev.Name, ev.OrderID, err)
		}
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusPending || got.VendorRef != "" {
		t.Errorf("status %s, ref %q", got.Status, got.VendorRef)
	}
	if n := len(eventsOf(read, "abs.unclear")); n != 5 {
		t.Errorf("%d unclear events, want 5", n)
	}
}

func TestRecipientsAreComparedAsOrdersStoreThem(t *testing.T) {
	for in, want := range map[string]string{
		"09073931630": "09073931630", " 09073931630 ": "09073931630", "+2349073931630": "09073931630",
		"2349073931630": "09073931630", "234 907 393 1630": "09073931630", "12345678901": "12345678901",
	} {
		if got := normalRecipient(in); got != want {
			t.Errorf("%q became %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int64{"99": 99_00, "99.00": 99_00, "1999.5": 1999_50, "0.01": 1} {
		if got, ok := koboOf(in); !ok || got != want {
			t.Errorf("%q is %d (%v), want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "abc", "NaN", "Inf"} {
		if _, ok := koboOf(in); ok {
			t.Errorf("%q should not be an amount", in)
		}
	}
}

// The whole path as ABS uses it: a signed delivery, in the shape ABS really sent on 2026-11-06.
func TestARealSignedDeliverySettlesThePendingOrderOverHTTP(t *testing.T) {
	f, _ := eventsFixture(t)
	srv := httptest.NewServer((&API{Service: f.svc, ABSWebhookSecret: "abs-secret"}).Routes())
	defer srv.Close()
	o := pendingFor(t, f, "hook-http", "09073931630")

	body := []byte(`{
 "event": "transaction.settled",
 "created_at": "2026-11-06T18:58:02+00:00",
 "data": {
  "order_id": "BA202611061957DSO1937JJFK254070W",
  "request_id": null,
  "status": "1",
  "amount": "99",
  "service": 2,
  "trans_kind": 3,
  "recipient": "09073931630",
  "message": "Bought 100 NGN Airtel Airtime Nigeria for 09073931630 and paid A99",
  "balance_before": "44142.00",
  "balance_after": "44043.00",
  "confirmed_at": "2026-11-06 19:58:00"
 }
}`)
	post := func(delivery string) int {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req, _ := http.NewRequest("POST", srv.URL+"/webhooks/abs", bytes.NewReader(body))
		req.Header.Set("X-ABS-Timestamp", ts)
		req.Header.Set("X-ABS-Signature", absSignature("abs-secret", ts, body))
		req.Header.Set("X-ABS-Delivery", delivery)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("dlv_1"); code != 200 {
		t.Fatalf("answered %d", code)
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusSuccess || got.VendorRef != "BA202611061957DSO1937JJFK254070W" {
		t.Errorf("status %s, ref %q", got.Status, got.VendorRef)
	}
	if code := post("dlv_1"); code != 200 { // ABS retries
		t.Errorf("the repeat answered %d", code)
	}
	if f.receiptsTo("a@b.co") != 1 {
		t.Errorf("%d receipts", f.receiptsTo("a@b.co"))
	}
	var stored int
	f.svc.Store.db.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE provider = 'abs'`).Scan(&stored)
	if stored != 2 {
		t.Errorf("%d webhooks stored, want both deliveries", stored)
	}

	// The admin sees ABS's id on the order.
	admin := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer admin.Close()
	resp, err := http.Get(admin.URL + "/admin/orders?status=SUCCESS")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if !strings.Contains(buf.String(), `"vendor_reference":"BA202611061957DSO1937JJFK254070W"`) {
		t.Errorf("the admin list does not show ABS's id: %s", buf.String())
	}
}

func TestAWebhookForAnOrderBeingBoughtGetsA503SoABSRetries(t *testing.T) {
	f, _ := eventsFixture(t)
	srv := httptest.NewServer((&API{Service: f.svc, ABSWebhookSecret: "abs-secret"}).Routes())
	defer srv.Close()
	o := orderWithKey(f, t, "hook-503")
	setCost(t, f, o.ID, 99_00)
	f.svc.Store.db.Exec(`UPDATE orders SET status = 'VENDING' WHERE id = ?`, o.ID)

	body := []byte(fmt.Sprintf(`{"event":"transaction.settled","data":{"order_id":"BA1","status":"1","amount":"99","recipient":%q,"message":"ok"}}`, o.Recipient))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, _ := http.NewRequest("POST", srv.URL+"/webhooks/abs", bytes.NewReader(body))
	req.Header.Set("X-ABS-Timestamp", ts)
	req.Header.Set("X-ABS-Signature", absSignature("abs-secret", ts, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("answered %d (Retry-After %q), want 503 with a Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// Without a secret the signature is not checked, so the message is saved and never acted on.
func TestAnUnverifiedABSWebhookSettlesNothing(t *testing.T) {
	f, _ := eventsFixture(t)
	srv := httptest.NewServer((&API{Service: f.svc}).Routes())
	defer srv.Close()
	o := pendingFor(t, f, "hook-unsigned", "09073931630")

	body := []byte(`{"event":"transaction.settled","data":{"order_id":"BA1","status":"1","amount":"99","recipient":"09073931630","message":"ok"}}`)
	resp, err := http.Post(srv.URL+"/webhooks/abs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("answered %d", resp.StatusCode)
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusPending || got.VendorRef != "" {
		t.Errorf("an unchecked webhook changed the order: %s, %q", got.Status, got.VendorRef)
	}
}

func TestMatchingAWebhookUsesTheIndexes(t *testing.T) {
	f := newFakes(t)
	rows, err := f.svc.Store.db.Query(`EXPLAIN QUERY PLAN SELECT id FROM orders WHERE vendor_ref = ? OR (vendor_ref = '' AND recipient = ? AND cost_kobo = ? AND created_at >= ? AND status IN (?,?))`,
		"X", "0801", 9900, 0, "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		plan = append(plan, d)
	}
	all := strings.Join(plan, "; ")
	if strings.Contains(all, "SCAN orders") || !strings.Contains(all, "orders_vendor_ref") || !strings.Contains(all, "orders_recipient") {
		t.Errorf("the lookup scans the whole table: %s", all)
	}
}

// A database from before ABS ids were kept gets the column and its indexes, and keeps its orders.
func TestOldDatabasesGetTheVendorRefColumn(t *testing.T) {
	path := t.TempDir() + "/old.db"
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	o := Order{IdempotencyKey: "k", Kind: KindAirtime, Network: "n", Recipient: "08012345678", AmountKobo: 100_00, CostKobo: 99_00, CustomerEmail: "a@b.co", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Insert(context.Background(), &o); err != nil {
		t.Fatal(err)
	}
	store.db.Exec(`DROP INDEX orders_vendor_ref`)
	if _, err := store.db.Exec(`ALTER TABLE orders DROP COLUMN vendor_ref`); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = OpenStore(path)
	if err != nil {
		t.Fatalf("an old database must be upgraded, not refused: %v", err)
	}
	defer store.Close()
	if ok, err := store.ClaimVendorRef(context.Background(), o.ID, "BA1"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if got, _ := store.ByID(context.Background(), o.ID); got.VendorRef != "BA1" || got.CostKobo != 99_00 {
		t.Errorf("%+v", got)
	}
}

func refundedHook(ref, recipient, amount, createdAt string) abs.Event {
	return abs.Event{Name: "refund.issued", Status: "6", OrderID: ref, Recipient: recipient, Amount: amount, CreatedAt: createdAt, Message: "Bought 1GB for NGN 430"}
}

func settledAt(ref, recipient, amount, createdAt string) abs.Event {
	ev := settledHook(ref, recipient, amount)
	ev.CreatedAt = createdAt
	return ev
}

func ownerSubjects(sent func() []string, contains string) int {
	n := 0
	for _, s := range sent() {
		if strings.Contains(s, contains) {
			n++
		}
	}
	return n
}

// 2026-11-11: ABS failed the attempt and refunded it, the owner bought it by hand, and then ABS
// "processed" the failed one after all.
func TestAPurchaseABSRefundedThatSettlesLaterRaisesAnAlarm(t *testing.T) {
	f, read := eventsFixture(t)
	_, sent := alertsOn(t, f)
	ctx := context.Background()
	failed := vendedOrder(t, f, "dbl-1", "07031849072", insufficient, 400)
	if failed.Status != StatusRefundDue {
		t.Fatalf("status %s", failed.Status)
	}

	// ABS: the attempt was refunded (ignored for orders, but remembered).
	if err := f.svc.SettleFromABS(ctx, refundedHook("BD-A", "07031849072", "99", "2026-11-11T00:04:02+00:00")); err != nil {
		t.Fatal(err)
	}
	// The owner's own purchase settles: matches the refund due order, which is only reported.
	if err := f.svc.SettleFromABS(ctx, settledAt("BD-B", "07031849072", "99", "2026-11-11T00:12:02+00:00")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, failed.ID); got.Status != StatusRefundDue || got.VendorRef != "BD-B" {
		t.Fatalf("status %s, ref %q", got.Status, got.VendorRef)
	}
	if ownerSubjects(sent, "ABS delivered an order recorded as REFUND_DUE") != 1 {
		t.Fatalf("the owner should have been told: %q", sent())
	}
	if len(eventsOf(read, "abs.redelivered")) != 0 {
		t.Fatal("nothing was redelivered yet")
	}

	// Later ABS settles the purchase it had refunded.
	if err := f.svc.SettleFromABS(ctx, settledAt("BD-A", "07031849072", "99", "2026-11-11T00:40:00+00:00")); err != nil {
		t.Fatal(err)
	}
	ev := eventsOf(read, "abs.redelivered")
	if len(ev) != 1 || ev[0].Level != LevelError || !strings.Contains(string(ev[0].Detail), `"abs_order":"BD-A"`) {
		t.Fatalf("events %+v", ev)
	}
	if ownerSubjects(sent, "ABS settled a purchase it had refunded") != 1 {
		t.Errorf("the owner was not emailed: %q", sent())
	}
	if got := orderNow(t, f, failed.ID); got.Status != StatusRefundDue || got.VendorRef != "BD-B" {
		t.Errorf("a redelivered purchase changed the order: %s, %q", got.Status, got.VendorRef)
	}
	// ABS delivering the same message again does not email again.
	f.svc.SettleFromABS(ctx, settledAt("BD-A", "07031849072", "99", "2026-11-11T00:40:00+00:00"))
	if ownerSubjects(sent, "ABS settled a purchase it had refunded") != 1 {
		t.Errorf("a repeat sent another email: %q", sent())
	}
}

func TestMessagesOutOfOrderOrRepeatedAreNotAlarms(t *testing.T) {
	f, read := eventsFixture(t)
	ctx := context.Background()
	hooks := []abs.Event{
		// A refund message that arrives after the settled one it is older than.
		settledAt("BD-1", "08011111111", "50", "2026-11-11T10:00:00+00:00"),
		refundedHook("BD-1", "08011111111", "50", "2026-11-11T09:00:00+00:00"),
		settledAt("BD-1", "08011111111", "50", "2026-11-11T10:00:00+00:00"),
		// Settled, then refunded, is a reversal, not a double charge.
		settledAt("BD-2", "08022222222", "50", "2026-11-11T10:00:00+00:00"),
		refundedHook("BD-2", "08022222222", "50", "2026-11-11T11:00:00+00:00"),
		// Failed, repeated, settled with no time on it is still after the failure.
	}
	for _, ev := range hooks {
		if err := f.svc.SettleFromABS(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(eventsOf(read, "abs.redelivered")); n != 0 {
		t.Errorf("%d alarms for harmless message orders", n)
	}
	// The record keeps the newest state.
	state := func(id string) string {
		var s string
		f.svc.Store.db.QueryRow(`SELECT state FROM abs_transactions WHERE abs_id = ?`, id).Scan(&s)
		return s
	}
	if state("BD-1") != "settled" || state("BD-2") != "refunded" {
		t.Errorf("recorded %q and %q", state("BD-1"), state("BD-2"))
	}
}

func TestAFailedThenSettledMessageWithoutATimeStillAlarms(t *testing.T) {
	f, read := eventsFixture(t)
	ctx := context.Background()
	f.svc.SettleFromABS(ctx, failedHook("BD-9", "08033333333", "50"))
	f.svc.SettleFromABS(ctx, settledHook("BD-9", "08033333333", "50"))
	if n := len(eventsOf(read, "abs.redelivered")); n != 1 {
		t.Errorf("%d alarms, want 1", n)
	}
}

// A settled message tied to a purchase while a pending order waits must not settle it.
func TestARedeliveredPurchaseNeverSettlesAPendingOrder(t *testing.T) {
	f, _ := eventsFixture(t)
	ctx := context.Background()
	pending := pendingFor(t, f, "dbl-pending", "09073931630")
	f.svc.SettleFromABS(ctx, refundedHook("BD-R", "09073931630", "99", "2026-11-11T00:00:00+00:00"))
	f.svc.SettleFromABS(ctx, settledAt("BD-R", "09073931630", "99", "2026-11-11T01:00:00+00:00"))
	if got := orderNow(t, f, pending.ID); got.Status != StatusPending {
		t.Errorf("the pending order became %s", got.Status)
	}
}

func TestASecondSettledPurchaseForAnOrderAlreadyTiedToOneIsReported(t *testing.T) {
	f, read := eventsFixture(t)
	_, sent := alertsOn(t, f)
	ctx := context.Background()
	o := settledFor(t, f, "twin-1", "09073931630")
	if err := f.svc.SettleFromABS(ctx, settledHook("BA-1", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, o.ID); got.VendorRef != "BA-1" {
		t.Fatalf("ref %q", got.VendorRef)
	}

	// The same message again is just the same purchase.
	f.svc.SettleFromABS(ctx, settledHook("BA-1", "09073931630", "99"))
	if n := len(eventsOf(read, "abs.duplicate")); n != 0 {
		t.Fatalf("a repeat of the same purchase was called a duplicate: %d", n)
	}

	// A different ABS purchase for the same recipient and amount is reported, and nothing changes.
	if err := f.svc.SettleFromABS(ctx, settledHook("BA-2", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	ev := eventsOf(read, "abs.duplicate")
	if len(ev) != 1 || ev[0].Level != LevelWarn || ev[0].OrderID != o.ID {
		t.Fatalf("events %+v", ev)
	}
	if ownerSubjects(sent, "Possible double purchase") != 1 {
		t.Errorf("the owner was not emailed: %q", sent())
	}
	if len(eventsOf(read, "abs.unmatched")) != 0 {
		t.Error("it was also reported as unmatched")
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusSuccess || got.VendorRef != "BA-1" {
		t.Errorf("the order changed: %s, %q", got.Status, got.VendorRef)
	}
	// A failed message is not a double delivery.
	f.svc.SettleFromABS(ctx, failedHook("BA-3", "09073931630", "99"))
	if n := len(eventsOf(read, "abs.duplicate")); n != 1 {
		t.Errorf("a failed message was reported as a duplicate")
	}
}

func TestOldABSRecordsAreForgotten(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	f.svc.Store.NoteABSState(ctx, "BD-old", "settled", time.Now())
	f.svc.Store.NoteABSState(ctx, "BD-new", "settled", time.Now())
	f.svc.Store.db.Exec(`UPDATE abs_transactions SET updated_at = ? WHERE abs_id = 'BD-old'`, time.Now().Add(-100*24*time.Hour).Unix())
	n, err := f.svc.Store.PurgeABSTransactions(ctx, time.Now().Add(-90*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	var left int
	f.svc.Store.db.QueryRow(`SELECT COUNT(*) FROM abs_transactions`).Scan(&left)
	if left != 1 {
		t.Errorf("%d records left, want 1", left)
	}
}

// A customer who buys the same airtime for the same number twice has two identical delivered orders. Their
// messages fit both, but neither order needs anything, so the owner is not warned about it.
func TestIdenticalDeliveredOrdersDoNotRaiseAnAmbiguityWarning(t *testing.T) {
	f, read := eventsFixture(t)
	a := settledFor(t, f, "same-a", "09073931630")
	b := settledFor(t, f, "same-b", "09073931630")
	for _, ref := range []string{"BA-1", "BA-2"} {
		if err := f.svc.SettleFromABS(context.Background(), settledHook(ref, "09073931630", "99")); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(eventsOf(read, "abs.ambiguous")); n != 0 {
		t.Errorf("%d ambiguity warnings for two orders that need nothing", n)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got := orderNow(t, f, id); got.Status != StatusSuccess {
			t.Errorf("%s became %s", id, got.Status)
		}
	}
}

// The owner bought a pending order again. A failed message could be about either purchase, so it never
// refunds the order, and never raises an alarm about an order that was delivered.
func TestAFailedMessageForAnOrderBoughtTwiceIsNotActedOn(t *testing.T) {
	f, read := eventsFixture(t)
	api := &API{Service: f.svc}
	srv := httptest.NewServer(api.AdminRoutes())
	defer srv.Close()
	ctx := context.Background()

	// Bought again and still unknown: a failed message must not make it a refund due.
	pending := pendingFor(t, f, "twice-pending", "09073931630")
	if code, _ := retryPost(t, srv, pending.ID, `{"confirm":true}`); code != 202 {
		t.Fatal(code)
	}
	api.Wait()
	if got := orderNow(t, f, pending.ID); got.Status != StatusPending || got.Attempts != 2 {
		t.Fatalf("status %s, attempts %d", got.Status, got.Attempts)
	}
	if err := f.svc.SettleFromABS(ctx, failedHook("BD-first", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, pending.ID); got.Status != StatusPending || got.VendorRef != "" {
		t.Errorf("a failed message settled an order bought twice: %s, %q", got.Status, got.VendorRef)
	}
	if len(eventsOf(read, "abs.unclear")) != 1 {
		t.Error("the owner should be told it could not be tied to a purchase")
	}
	// A settled message is still good news whichever purchase it is about.
	if err := f.svc.SettleFromABS(ctx, settledHook("BD-second", "09073931630", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, pending.ID); got.Status != StatusSuccess || got.VendorRef != "BD-second" {
		t.Errorf("status %s, ref %q", got.Status, got.VendorRef)
	}

	// Failed first, then bought again and delivered: the first purchase's late failed message is no alarm.
	failed := vendedOrder(t, f, "twice-failed", "07031849072", insufficient, 400)
	f.absFails(absSuccess, 200)
	if code, _ := retryPost(t, srv, failed.ID, `{}`); code != 202 {
		t.Fatal(code)
	}
	api.Wait()
	if err := f.svc.SettleFromABS(ctx, failedHook("BD-late", "07031849072", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, failed.ID); got.Status != StatusSuccess || got.VendorRef != "" {
		t.Errorf("status %s, ref %q", got.Status, got.VendorRef)
	}
	if n := len(eventsOf(read, "abs.conflict")); n != 0 {
		t.Errorf("%d conflict alarms for the earlier purchase failing", n)
	}
}
