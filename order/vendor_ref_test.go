package order

import "testing"

// ABS is told our order id as request_id, so a purchase can be found afterwards even when its reply
// never arrived.
func TestABSIsToldWhichOrderThePurchaseIsFor(t *testing.T) {
	f := newFakes(t)
	o := f.payAndVend(t, orderWithKey(f, t, "ref-1"))
	if o.Status != StatusSuccess {
		t.Fatalf("status %s", o.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.spendForm.Get("request_id"); got != o.ID {
		t.Errorf("ABS was sent request_id %q, want the order id %q", got, o.ID)
	}
}
