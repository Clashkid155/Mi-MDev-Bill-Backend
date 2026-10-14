package kora

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Bills-Backend/payment"
)

func sign(secret, data string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func headerOf(sig string) http.Header {
	h := http.Header{}
	h.Set(SignatureHeader, sig)
	return h
}

func TestVerifyWebhook(t *testing.T) {
	const secret = "sk_test_abc"
	data := `{"reference":"MDEV_1","amount":1000,"status":"success"}`
	body := []byte(`{"event":"charge.success","data":` + data + `}`)

	if err := VerifyWebhook(secret, headerOf(sign(secret, data)), body); err != nil {
		t.Errorf("valid signature rejected: %v", err)
	}
	// Only data is signed: the event name and spacing around it do not matter.
	spaced := []byte("{ \"event\" : \"charge.success\",\n \"data\": " + data + " }")
	if err := VerifyWebhook(secret, headerOf(sign(secret, data)), spaced); err != nil {
		t.Errorf("valid signature rejected when the envelope is re-spaced: %v", err)
	}
	// A signature over the compact form still matches when the data was re-spaced in transit.
	respaced := []byte(`{"event":"charge.success","data":{ "reference": "MDEV_1", "amount": 1000, "status": "success" }}`)
	if err := VerifyWebhook(secret, headerOf(sign(secret, data)), respaced); err != nil {
		t.Errorf("compact fallback failed: %v", err)
	}
	// Uppercase hex is the same signature.
	if err := VerifyWebhook(secret, headerOf(upper(sign(secret, data))), body); err != nil {
		t.Errorf("uppercase hex rejected: %v", err)
	}

	bad := map[string]struct {
		secret string
		header http.Header
		body   []byte
	}{
		"wrong secret":          {"other", headerOf(sign(secret, data)), body},
		"signed other data":     {secret, headerOf(sign(secret, `{"reference":"MDEV_2"}`)), body},
		"tampered amount":       {secret, headerOf(sign(secret, data)), []byte(`{"event":"charge.success","data":{"reference":"MDEV_1","amount":1,"status":"success"}}`)},
		"no header":             {secret, http.Header{}, body},
		"not hex":               {secret, headerOf("zzzz"), body},
		"no data":               {secret, headerOf(sign(secret, data)), []byte(`{"event":"charge.success"}`)},
		"not json":              {secret, headerOf(sign(secret, data)), []byte(`nope`)},
		"empty secret":          {"", headerOf(sign("", data)), body},
		"whole body was signed": {secret, headerOf(sign(secret, string(body))), body},
	}
	for name, c := range bad {
		if err := VerifyWebhook(c.secret, c.header, c.body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func serverReplying(t *testing.T, status int, body string) (*Client, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", " sk_test_abc "), &got
}

func TestCreateChargeReadsTheAccount(t *testing.T) {
	c, req := serverReplying(t, 200, `{"status":true,"message":"ok","data":{"amount":1500,"amount_expected":"1500.00","reference":"MDEV_1",
		"bank_account":{"account_name":"Mi MDev","account_number":"7590031627","bank_name":"wema","expiry_date_in_utc":"2030-06-05T16:58:09.193Z"}}}`)
	ch, err := c.CreateCharge(context.Background(), payment.ChargeRequest{Reference: "MDEV_1", AmountKobo: 1500_00, CustomerEmail: "a@b.co"})
	if err != nil {
		t.Fatal(err)
	}
	if ch.AccountNo != "7590031627" || ch.BankName != "wema" || ch.TxnRef != "MDEV_1" ||
		!ch.ExpiresAt.Equal(time.Date(2030, 6, 5, 16, 58, 9, 193e6, time.UTC)) {
		t.Fatalf("charge %+v", ch)
	}
	if req.URL.Path != "/api/v1/charges/bank-transfer" || req.Header.Get("Authorization") != "Bearer sk_test_abc" {
		t.Fatalf("path %q auth %q", req.URL.Path, req.Header.Get("Authorization"))
	}
}

func TestCreateChargeErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"kora refuses":      {400, `{"status":false,"message":"Invalid reference"}`},
		"success false 200": {200, `{"status":false,"message":"Bank transfer not enabled"}`},
		"no account":        {200, `{"status":true,"data":{"amount":15,"reference":"MDEV_1"}}`},
		"other amount":      {200, `{"status":true,"data":{"amount":15,"amount_expected":16.5,"bank_account":{"account_number":"1"}}}`},
	}
	for name, tc := range cases {
		c, _ := serverReplying(t, tc.status, tc.body)
		if _, err := c.CreateCharge(context.Background(), payment.ChargeRequest{Reference: "MDEV_1", AmountKobo: 15_00}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	c, _ := serverReplying(t, 400, `{"status":false,"message":"Invalid reference"}`)
	_, err := c.CreateCharge(context.Background(), payment.ChargeRequest{Reference: "x", AmountKobo: 100})
	if e, ok := err.(*Error); !ok || e.HTTPStatus != 400 || e.Message != "Invalid reference" {
		t.Errorf("error %#v", err)
	}
}

func TestVerifyUsesAmountAccepted(t *testing.T) {
	cases := []struct {
		name string
		body string
		want payment.Payment
	}{
		{"paid in full", `{"status":true,"data":{"status":"success","amount":1000,"amount_paid":1000,"amount_accepted":1000}}`,
			payment.Payment{Status: payment.Paid, ReceivedKobo: 1000_00, Detail: "success"}},
		{"customer overpaid, excess returned", `{"status":true,"data":{"status":"success","amount":1000,"amount_paid":2500,"amount_accepted":1000}}`,
			payment.Payment{Status: payment.Paid, ReceivedKobo: 1000_00, Detail: "success"}},
		{"overpayment accepted", `{"status":true,"data":{"status":"success","amount":1000,"amount_paid":2500,"amount_accepted":2500}}`,
			payment.Payment{Status: payment.Paid, ReceivedKobo: 2500_00, Detail: "success"}},
		{"string amounts", `{"status":true,"data":{"status":"success","amount_accepted":"22.50"}}`,
			payment.Payment{Status: payment.Paid, ReceivedKobo: 22_50, Detail: "success"}},
		{"no accepted field falls back to paid", `{"status":true,"data":{"status":"success","amount_paid":300}}`,
			payment.Payment{Status: payment.Paid, ReceivedKobo: 300_00, Detail: "success"}},
		{"processing", `{"status":true,"data":{"status":"processing","amount":1000,"amount_paid":0,"amount_accepted":0}}`,
			payment.Payment{Status: payment.Pending, Detail: "processing"}},
		{"failed", `{"status":true,"data":{"status":"failed"}}`, payment.Payment{Status: payment.Failed, Detail: "failed"}},
		{"unknown word is not money", `{"status":true,"data":{"status":"abandoned","amount_accepted":1000}}`,
			payment.Payment{Status: payment.Pending, ReceivedKobo: 1000_00, Detail: "abandoned"}},
	}
	for _, tc := range cases {
		c, req := serverReplying(t, 200, tc.body)
		got, err := c.Verify(context.Background(), "MDEV_1", "ignored")
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
		if req.URL.Path != "/api/v1/charges/MDEV_1" || req.Method != "GET" {
			t.Errorf("%s: %s %s", tc.name, req.Method, req.URL.Path)
		}
	}
}

func TestExistingChargeCannotBeRecovered(t *testing.T) {
	c := New("http://unused", "k")
	if _, err := c.ExistingCharge(context.Background(), "MDEV_1"); err != payment.ErrNotRecoverable {
		t.Fatalf("got %v", err)
	}
}
