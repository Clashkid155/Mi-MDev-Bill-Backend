package order

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Bills-Backend/platform"
)

// stopper is a stand-in for the notices: it stops new orders when told to, and remembers who asked.
type stopper struct {
	on     atomic.Bool
	until  int64
	caller atomic.Value // platform.Caller
}

func (s *stopper) OrdersBlocked(_ context.Context, c platform.Caller) (platform.Block, bool) {
	s.caller.Store(c)
	if !s.on.Load() {
		return platform.Block{}, false
	}
	return platform.Block{NoticeID: 7, Title: "Back at 3am", Message: "We are upgrading. Orders resume at 3am.", Until: s.until}, true
}

func TestNewOrdersAreRefusedWhileMaintenanceBlocksThem(t *testing.T) {
	f := newFakes(t)
	gate := &stopper{until: time.Now().Add(2 * time.Hour).Unix()}
	srv := httptest.NewServer((&API{Service: f.svc, Gate: gate}).Routes())
	defer srv.Close()

	// Before: orders go through, and the app's identity reaches the gate.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/orders", strings.NewReader(mustJSON(airtimeRequest)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-App", "mi-mdev")
	req.Header.Set("X-Platform", "android")
	req.Header.Set("X-App-Version", "1.4.2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("before maintenance an order answered %d", resp.StatusCode)
	}
	if got, _ := gate.caller.Load().(platform.Caller); got != (platform.Caller{App: "mi-mdev", Platform: "android", Version: "1.4.2"}) {
		t.Errorf("the gate was told the caller is %+v", got)
	}

	// During: a different, new order is refused with 503, a code the app can act on and when to come back.
	gate.on.Store(true)
	fresh := airtimeRequest
	fresh.IdempotencyKey = "a-brand-new-key-0001"
	invoices := f.invoicesMade.Load()
	code, body := postJSON(t, srv.URL+"/v1/orders", fresh)
	var refusal struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Code     string `json:"code"`
			NoticeID int64  `json:"notice_id"`
			Title    string `json:"title"`
			Until    int64  `json:"until"`
		} `json:"data"`
	}
	json.Unmarshal(body, &refusal)
	if code != 503 || refusal.Status != "failed" || refusal.Error != "We are upgrading. Orders resume at 3am." ||
		refusal.Data.Code != "maintenance" || refusal.Data.NoticeID != 7 || refusal.Data.Title != "Back at 3am" || refusal.Data.Until != gate.until {
		t.Errorf("during maintenance: %d %s", code, body)
	}
	if f.invoicesMade.Load() != invoices {
		t.Error("a refused order must not create an invoice")
	}
	if o, err := f.svc.Store.ByIdempotencyKey(context.Background(), fresh.IdempotencyKey); err == nil {
		t.Errorf("a refused order must not be stored: %+v", o)
	}
	resp2, _ := http.Post(srv.URL+"/v1/orders", "application/json", strings.NewReader(mustJSON(fresh)))
	resp2.Body.Close()
	if wait, err := strconv.Atoi(resp2.Header.Get("Retry-After")); err != nil || wait < 1 || wait > 3600 {
		t.Errorf("when the end is known the answer says when to retry, within the hour: %q", resp2.Header.Get("Retry-After"))
	}

	// An app retrying an order it already made still gets that order back (it is not a new order).
	code, body = postJSON(t, srv.URL+"/v1/orders", airtimeRequest)
	var replay orderReply
	json.Unmarshal(body, &replay)
	if code != 200 || replay.Data.ID == "" {
		t.Errorf("a retry of an existing order must still work during maintenance: %d %s", code, body)
	}

	// Looking at orders is never blocked.
	if code, _ := getBody(t, srv.URL+"/v1/orders/"+replay.Data.ID); code != 200 {
		t.Errorf("checking an order during maintenance answered %d", code)
	}

	// After: orders flow again.
	gate.on.Store(false)
	if code, body := postJSON(t, srv.URL+"/v1/orders", fresh); code != 200 {
		t.Errorf("after maintenance a new order answered %d: %s", code, body)
	}
}

func TestAPaidOrderIsStillDeliveredDuringMaintenance(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	o, err := f.svc.Create(ctx, airtimeRequest)
	if err != nil {
		t.Fatal(err)
	}
	gate := &stopper{}
	gate.on.Store(true) // maintenance starts after the customer ordered, before they paid
	api := &API{Service: f.svc, Gate: gate}

	f.svc.Store.db.Exec(`UPDATE orders SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), o.ID)
	f.invoiceState.Store("PAID")
	f.invoiceKobo.Store(o.Due())
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { api.Sweep(runCtx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status == StatusSuccess {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-done
	api.Wait()
	if got, _ := f.svc.Store.ByID(ctx, o.ID); got.Status != StatusSuccess {
		t.Errorf("an order paid during maintenance is %s, want SUCCESS: money taken must always be delivered", got.Status)
	}
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Bills must not import notices: it asks platform.Gate, and main wires the notices to it.
func TestOrderDoesNotDependOnNotices(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "Bills-Backend/notify" {
				t.Errorf("%s imports notify: bills must reach notices through platform.Gate", f)
			}
		}
	}
}
