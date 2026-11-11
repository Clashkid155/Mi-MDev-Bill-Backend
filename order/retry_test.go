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

	"Bills-Backend/platform"
)

func retryPost(t *testing.T, srv *httptest.Server, id, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/admin/orders/"+id+"/retry", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func retryFixture(t *testing.T) (*fakes, *API, *httptest.Server, func(platform.EventFilter) []platform.Event) {
	t.Helper()
	f, read := eventsFixture(t)
	api := &API{Service: f.svc}
	srv := httptest.NewServer(api.AdminRoutes())
	t.Cleanup(srv.Close)
	return f, api, srv, read
}

func TestTheOwnerCanBuyAFailedOrderAgainAndTheCustomerGetsTheReceipt(t *testing.T) {
	f, api, srv, read := retryFixture(t)
	failed := vendedOrder(t, f, "retry-1", "07031849072", insufficient, 400)
	if failed.Status != StatusRefundDue {
		t.Fatalf("status %s", failed.Status)
	}
	f.svc.Store.db.Exec(`UPDATE orders SET vendor_ref = 'BD-first' WHERE id = ?`, failed.ID)
	f.absFails(absSuccess, 200) // ABS is fine this time

	code, out := retryPost(t, srv, failed.ID, `{"note":" ABS had a bad minute "}`)
	data, _ := out["data"].(map[string]any)
	if code != 202 || data["status"] != "VENDING" || data["attempts"] != float64(2) {
		t.Fatalf("%d %v", code, out)
	}
	api.Wait()

	got := orderNow(t, f, failed.ID)
	if got.Status != StatusSuccess || got.Attempts != 2 {
		t.Errorf("status %s, attempts %d", got.Status, got.Attempts)
	}
	if got.VendorRef != "" {
		t.Errorf("the first purchase's ABS id %q was kept: the new purchase could not claim it", got.VendorRef)
	}
	if f.receiptsTo("a@b.co") != 1 {
		t.Errorf("the customer got %d receipts, want 1", f.receiptsTo("a@b.co"))
	}
	if f.spendCalls.Load() != 2 {
		t.Errorf("ABS was called %d times, want 2 (the first attempt and the repeat)", f.spendCalls.Load())
	}
	f.mu.Lock()
	ref := f.spendForm.Get("request_id")
	f.mu.Unlock()
	if ref != failed.ID+"-2" {
		t.Errorf("the repeat purchase was sent request_id %q, want %q so it differs from the first", ref, failed.ID+"-2")
	}
	events := read(platform.EventFilter{OrderID: failed.ID, Kind: "order.retried"})
	if len(events) != 1 || events[0].Level != LevelWarn || !strings.Contains(events[0].Message, "attempt 2") || !strings.Contains(events[0].Message, "ABS had a bad minute") {
		t.Errorf("events %+v", events)
	}
}

func TestABuyAgainThatFailsAgainLeavesTheRefundDueAndCanBeTriedOnceMore(t *testing.T) {
	f, api, srv, _ := retryFixture(t)
	failed := vendedOrder(t, f, "retry-2", "07031849072", insufficient, 400)

	for want := 2; want <= 3; want++ {
		if code, _ := retryPost(t, srv, failed.ID, ``); code != 202 {
			t.Fatalf("attempt %d answered %d", want, code)
		}
		api.Wait()
		got := orderNow(t, f, failed.ID)
		if got.Status != StatusRefundDue || got.Attempts != want {
			t.Fatalf("after attempt %d: status %s, attempts %d", want, got.Status, got.Attempts)
		}
	}
	if f.receiptsTo("a@b.co") != 0 {
		t.Error("no receipt for an order that failed")
	}
}

func TestBuyingAPendingOrderAgainNeedsConfirmation(t *testing.T) {
	f, api, srv, _ := retryFixture(t)
	pending := pendingFor(t, f, "retry-3", "09073931630")
	f.absFails(absSuccess, 200)

	code, out := retryPost(t, srv, pending.ID, `{}`)
	if code != 400 || !strings.Contains(out["error"].(string), "twice") {
		t.Fatalf("unconfirmed: %d %v", code, out)
	}
	if got := orderNow(t, f, pending.ID); got.Status != StatusPending || f.spendCalls.Load() != 1 {
		t.Fatalf("an unconfirmed retry changed things: %s, %d calls", got.Status, f.spendCalls.Load())
	}
	if code, _ := retryPost(t, srv, pending.ID, `{"confirm":true,"note":"checked ABS, nothing arrived"}`); code != 202 {
		t.Fatalf("confirmed: %d", code)
	}
	api.Wait()
	if got := orderNow(t, f, pending.ID); got.Status != StatusSuccess || got.Attempts != 2 || f.spendCalls.Load() != 2 {
		t.Errorf("status %s, attempts %d, %d calls", got.Status, got.Attempts, f.spendCalls.Load())
	}
}

func TestOnlyPendingAndRefundDueOrdersCanBeBoughtAgain(t *testing.T) {
	f, _, srv, _ := retryFixture(t)
	delivered := settledFor(t, f, "retry-ok", "08011111111")
	waiting := orderWithKey(f, t, "retry-wait")
	refunded := refundDue(t, f, "retry-refunded")
	if code, _ := postRefunded(t, srv, refunded.ID, `{"note":"sent"}`); code != 200 {
		t.Fatal(code)
	}
	calls := f.spendCalls.Load()

	for name, id := range map[string]string{"a delivered order": delivered.ID, "an unpaid order": waiting.ID, "a refunded order": refunded.ID} {
		if code, out := retryPost(t, srv, id, `{"confirm":true}`); code != 409 || !strings.Contains(out["error"].(string), "this one is") {
			t.Errorf("%s: %d %v, want 409", name, code, out)
		}
	}
	if code, _ := retryPost(t, srv, "MDEV_nothere", `{}`); code != 404 {
		t.Errorf("an unknown order: %d, want 404", code)
	}
	if code, _ := retryPost(t, srv, delivered.ID, `nope`); code != 400 {
		t.Errorf("a bad body: %d, want 400", code)
	}
	if f.spendCalls.Load() != calls {
		t.Error("a refused retry still called ABS")
	}
}

// A double click must buy once, not twice: the customer's data would be paid for twice.
func TestTwoClicksAtOnceBuyOnce(t *testing.T) {
	f, api, srv, _ := retryFixture(t)
	failed := vendedOrder(t, f, "retry-race", "07031849072", insufficient, 400)
	f.absFails(absSuccess, 200)
	before := f.spendCalls.Load()

	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := retryPost(t, srv, failed.ID, `{"confirm":true}`)
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	api.Wait()
	accepted := 0
	for c := range codes {
		switch c {
		case 202:
			accepted++
		case 409:
		default:
			t.Errorf("unexpected answer %d", c)
		}
	}
	if accepted != 1 {
		t.Errorf("%d clicks were accepted, want 1", accepted)
	}
	if n := f.spendCalls.Load() - before; n != 1 {
		t.Errorf("ABS was called %d times for 8 clicks, want 1", n)
	}
	if got := orderNow(t, f, failed.ID); got.Attempts != 2 || got.Status != StatusSuccess {
		t.Errorf("attempts %d, status %s", got.Attempts, got.Status)
	}
	if f.receiptsTo("a@b.co") != 1 {
		t.Errorf("%d receipts", f.receiptsTo("a@b.co"))
	}
}

func TestAnOrderBoughtAgainDoesNotLookLikeADoublePurchaseWhenItsWebhookArrives(t *testing.T) {
	f, api, srv, read := retryFixture(t)
	failed := vendedOrder(t, f, "retry-hook", "07031849072", insufficient, 400)
	ctx := context.Background()
	// A message about the failed first purchase had been matched to the order.
	f.svc.SettleFromABS(ctx, failedHook("BD-first", "07031849072", "99"))
	f.absFails(absSuccess, 200)
	if code, _ := retryPost(t, srv, failed.ID, `{}`); code != 202 {
		t.Fatal(code)
	}
	api.Wait()

	if err := f.svc.SettleFromABS(ctx, settledHook("BD-second", "07031849072", "99")); err != nil {
		t.Fatal(err)
	}
	if got := orderNow(t, f, failed.ID); got.VendorRef != "BD-second" || got.Status != StatusSuccess {
		t.Errorf("ref %q, status %s", got.VendorRef, got.Status)
	}
	if n := len(read(platform.EventFilter{Kind: "abs.duplicate"})) + len(read(platform.EventFilter{Kind: "abs.conflict"})); n != 0 {
		t.Errorf("the new purchase's own webhook raised %d warnings", n)
	}
}
