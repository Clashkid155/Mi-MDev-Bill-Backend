package abs

import (
	"net/http"
	"testing"
)

// A transaction.settled message as ABS really sent it on 2026-11-06 (the recipient is swapped for
// a dummy number). Note request_id is null, amount is a string, and the message holds an emoji.
func TestParseAWebhookABSReallySent(t *testing.T) {
	body := []byte(`{
 "event": "transaction.settled",
 "created_at": "2026-11-06T18:28:02+00:00",
 "data": {
  "order_id": "BD202611061842GVY1767JREA253936W",
  "request_id": null,
  "status": "1",
  "amount": "999",
  "service": 1,
  "trans_kind": 3,
  "recipient": "08012345678",
  "message": "Bought 2GB HOT DEAL 🔥 20 - 30 Days validity for NGN 999",
  "balance_before": "45141.00",
  "balance_after": "44142.00",
  "confirmed_at": "2026-11-06 18:42:44"
 }
}`)
	h := http.Header{}
	h.Set("X-ABS-Delivery", "dlv_1")
	ev, err := ParseWebhook(h, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "transaction.settled" || ev.OrderID != "BD202611061842GVY1767JREA253936W" || ev.Status != "1" || ev.Amount != "999" ||
		ev.Recipient != "08012345678" || ev.ConfirmedAt != "2026-11-06 18:42:44" || ev.DeliveryID != "dlv_1" {
		t.Errorf("%+v", ev)
	}
}

// A number where ABS usually sends text must still be read.
func TestParseAWebhookWithNumbersForStatusAndAmount(t *testing.T) {
	ev, err := ParseWebhook(http.Header{}, []byte(`{"event":"transaction.failed","data":{"order_id":"BA1","status":3,"amount":99.5,"recipient":"08012345678"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != "3" || ev.Amount != "99.5" || ev.Name != "transaction.failed" {
		t.Errorf("%+v", ev)
	}
	if _, err := ParseWebhook(http.Header{}, []byte(`{"event":"x","data":{"status":{"a":1}}}`)); err == nil {
		t.Error("an object where a status belongs should not be read")
	}
}
