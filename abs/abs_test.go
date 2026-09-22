package abs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeABS(t *testing.T, status int, body string, check func(*http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Error("missing bearer key")
		}
		if check != nil {
			check(r)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "key", false)
}

func TestAirtimeSendsFormFields(t *testing.T) {
	c := fakeABS(t, 200, `{"status":true,"text":"Successful","data":{}}`, func(r *http.Request) {
		r.ParseForm()
		if r.URL.Path != "/api/user/transaction/buy_airtime.php" ||
			r.Form.Get("airtimetid") != "MTNAIR6TFD" || r.Form.Get("phoneno") != "08012345678" || r.Form.Get("amount") != "100" {
			t.Errorf("unexpected request %s %v", r.URL.Path, r.Form)
		}
	})
	if res := c.Airtime(context.Background(), "MTNAIR6TFD", "08012345678", 10000); res.State != Success {
		t.Fatalf("%+v", res)
	}
}

func TestSandboxFlag(t *testing.T) {
	c := fakeABS(t, 200, `{"status":true}`, func(r *http.Request) {
		r.ParseForm()
		if r.Form.Get("sandbox") != "1" {
			t.Error("sandbox flag not sent")
		}
	})
	c.sandbox = true
	c.Data(context.Background(), "MTN56TFD", "6BO1H", "0801")
}

func TestOutcomeClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   State
	}{
		{"ok", 200, `{"status":true,"text":"Successful"}`, Success},
		{"rejected by ABS", 200, `{"status":false,"text":"bad","error":{"code":141,"text":"invalid"}}`, Failed},
		{"bad request", 400, `{"status":false}`, Failed},
		{"rate limited", 429, ``, Failed},
		{"server error", 502, `<html>`, Processing},
		{"unreadable 200", 200, `not json`, Processing},
		{"no status field", 200, `{}`, Processing},
	}
	for _, tc := range cases {
		c := fakeABS(t, tc.status, tc.body, nil)
		if res := c.Data(context.Background(), "MTN56TFD", "6BO1H", "0801"); res.State != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, res.State, tc.want)
		}
	}
}

func TestNetworkErrorIsProcessing(t *testing.T) {
	c := New("http://127.0.0.1:1", "key", false) // connection refused
	if res := c.Data(context.Background(), "n", "p", "0801"); res.State != Processing {
		t.Fatalf("a network error must be PROCESSING, got %s", res.State)
	}
}

func TestInvalidOrdersNeverReachABS(t *testing.T) {
	c := New("http://127.0.0.1:1", "key", false)
	for name, res := range map[string]Result{
		"fractional naira": c.Airtime(context.Background(), "n", "0801", 10050),
		"zero":             c.Electricity(context.Background(), "d", "123", 0),
		"31 vouchers":      c.Voucher(context.Background(), "n", "u", 31),
	} {
		if res.State != Failed {
			t.Errorf("%s: got %s, want FAILED", name, res.State)
		}
	}
}

func TestWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"event":"transaction.settled","data":{"order_id":"BD1","status":"1","amount":"500.00","recipient":"0801"}}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	h := http.Header{}
	h.Set("X-ABS-Timestamp", ts)
	h.Set("X-ABS-Signature", webhookSignature("sec", ts, body))
	h.Set("X-ABS-Delivery", "d1")

	if err := VerifyWebhook("sec", h, body, now); err != nil {
		t.Fatal(err)
	}
	if err := VerifyWebhook("wrong", h, body, now); err == nil {
		t.Error("wrong secret accepted")
	}
	if err := VerifyWebhook("sec", h, append(body, ' '), now); err == nil {
		t.Error("tampered body accepted")
	}
	if err := VerifyWebhook("sec", h, body, now.Add(10*time.Minute)); err == nil {
		t.Error("stale timestamp accepted")
	}
	ev, err := ParseWebhook(h, body)
	if err != nil || ev.OrderID != "BD1" || ev.DeliveryID != "d1" || ev.Status != "1" {
		t.Fatalf("%v %+v", err, ev)
	}
}

func TestDirectRoutesPassABSReplyThrough(t *testing.T) {
	c := fakeABS(t, 200, `{"status":true,"text":"Successful","data":{"final_amount":100}}`, nil)
	mux := http.NewServeMux()
	c.RegisterDirectRoutes(mux)

	req := httptest.NewRequest("POST", "/abs/airtime", strings.NewReader(`{"network":"MTNAIR6TFD","phone":"0801","amount_naira":100}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var got struct {
		Status string `json:"status"`
		Data   struct {
			State State `json:"state"`
			Raw   struct {
				Data struct {
					FinalAmount int `json:"final_amount"`
				} `json:"data"`
			} `json:"raw"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil ||
		got.Status != "successful" || got.Data.State != Success || got.Data.Raw.Data.FinalAmount != 100 {
		t.Fatalf("%v %s", err, rec.Body)
	}
}

// A success reply whose "error" field is not an object must still parse.
func TestSystemDataToleratesOddErrorField(t *testing.T) {
	for _, errField := range []string{`[]`, `null`, `""`, `false`, `"none"`} {
		body := `{"status":true,"text":"Data found","time":"21-09-26","error":` + errField + `,"data":{"airtimeproviders":[]}}`
		data, err := fakeABS(t, 200, body, nil).SystemData(context.Background())
		if err != nil || string(data) != `{"airtimeproviders":[]}` {
			t.Errorf("error=%s: %v %s", errField, err, data)
		}
		if res := fakeABS(t, 200, body, nil).Data(context.Background(), "n", "p", "0801"); res.State != Success {
			t.Errorf("error=%s: purchase reported %s", errField, res.State)
		}
	}
}

func TestCheckTransaction(t *testing.T) {
	for body, want := range map[string]State{
		`{"Status":"successful"}`: Success,
		`{"Status":"Processing"}`: Processing,
		`{"Status":"failed"}`:     Failed,
		`{"Status":"Refund"}`:     Failed,
	} {
		c := fakeABS(t, 200, body, func(r *http.Request) {
			if r.URL.Path != "/api/partner/v1/transaction.php" || r.URL.Query().Get("ident") != "BD 1" {
				t.Errorf("unexpected request %s", r.URL)
			}
		})
		res, err := c.CheckTransaction(context.Background(), "BD 1")
		if err != nil || res.State != want {
			t.Errorf("%s: %v %s", body, err, res.State)
		}
	}
	if _, err := fakeABS(t, 500, `oops`, nil).CheckTransaction(context.Background(), "BD1"); err == nil {
		t.Error("a 5xx must be an error, not an order state")
	}
}
