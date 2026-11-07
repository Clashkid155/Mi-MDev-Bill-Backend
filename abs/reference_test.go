package abs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Our own reference travels with every kind of purchase, and is left out when there is none.
func TestEveryPurchaseCarriesOurReference(t *testing.T) {
	var mu sync.Mutex
	sent := map[string]string{} // path -> request_id (and whether the field was present at all)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		if _, present := r.Form["request_id"]; present {
			sent[r.URL.Path] = r.Form.Get("request_id")
		} else {
			sent[r.URL.Path] = "(none)"
		}
		mu.Unlock()
		w.Write([]byte(`{"status":true,"text":"Successful","data":{}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "key", false)
	ctx := context.Background()

	c.Airtime(ctx, "MTNAIR6TFD", "08012345678", 10000, "MDEV_AIRTIME1")
	c.Data(ctx, "MTN56TFD", "6BO1H", "08012345678", "MDEV_DATA1")
	c.Electricity(ctx, "disco", "12345678901", 200000, "MDEV_POWER1")
	c.Voucher(ctx, "MTNAIR6TFD", "HBSI2", 2, "MDEV_VOUCHER1")
	for path, want := range map[string]string{
		pathBuyAirtime: "MDEV_AIRTIME1", pathBuyData: "MDEV_DATA1", pathBuyElectric: "MDEV_POWER1", pathBuyVoucher: "MDEV_VOUCHER1",
	} {
		if sent[path] != want {
			t.Errorf("%s sent request_id %q, want %q", path, sent[path], want)
		}
	}

	c.Data(ctx, "MTN56TFD", "6BO1H", "08012345678", "")
	if sent[pathBuyData] != "(none)" {
		t.Errorf("with no reference the field must be left out, got %q", sent[pathBuyData])
	}
}

// The reference is part of the sandbox call too, so ABS can be asked to validate it without a purchase.
func TestTheReferenceIsSentInSandboxToo(t *testing.T) {
	var form map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.Form
		w.Write([]byte(`{"status":true,"text":"Successful","sandbox":true}`))
	}))
	defer srv.Close()
	New(srv.URL, "key", true).Data(context.Background(), "n", "p", "0801", "MDEV_SANDBOX1")
	if strings.Join(form["request_id"], "") != "MDEV_SANDBOX1" || strings.Join(form["sandbox"], "") != "1" {
		t.Errorf("form %v", form)
	}
}

// Purchases are given far longer than anything else: ABS itself waits about a minute for the provider
// before it answers. (A 32 second purchase on 2026-11-06 was cut off at 30 and left an order PENDING.)
func TestPurchasesWaitLongerThanOtherCalls(t *testing.T) {
	c := New("http://unused", "key", false)
	if c.http.Timeout != 30*time.Second || c.spend.Timeout != 90*time.Second {
		t.Errorf("timeouts: %v for ordinary calls, %v for purchases", c.http.Timeout, c.spend.Timeout)
	}
	for _, path := range []string{pathBuyAirtime, pathBuyData, pathBuyElectric, pathBuyVoucher} {
		if !isSpend(path) {
			t.Errorf("%s should take the purchase timeout", path)
		}
	}
	for _, path := range []string{pathUserDetail, pathTransaction, "/api/user/system/get_system_data.php", "/api/user/transaction/validate_meter.php"} {
		if isSpend(path) {
			t.Errorf("%s must keep the short timeout", path)
		}
	}

	// And it really is the purchase client that is used: with tiny timeouts a slow server loses the
	// ordinary call but the purchase still gets its answer.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"status":true,"text":"Successful","data":{}}`))
	}))
	defer slow.Close()
	c = New(slow.URL, "key", false)
	c.http.Timeout, c.spend.Timeout = 50*time.Millisecond, 5*time.Second
	if _, err := c.Balance(context.Background()); err == nil {
		t.Error("an ordinary call should have timed out")
	}
	if res := c.Data(context.Background(), "n", "p", "0801", "MDEV_SLOW"); res.State != Success {
		t.Errorf("the purchase should have waited for the slow answer: %+v", res)
	}
}
