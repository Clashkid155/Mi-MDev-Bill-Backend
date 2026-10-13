package monnify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateInvoiceAndTokenReuse(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			logins.Add(1)
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"requestSuccessful":true,"responseMessage":"success","responseCode":"0","responseBody":{"accessToken":"tok","expiresIn":3600}}`))
		case "/api/v1/invoice/create":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if in["amount"].(float64) != 290 || in["contractCode"] != "CC" || in["expiryDate"] != "2030-01-02 13:00:00" {
				t.Errorf("unexpected body: %v", in)
			}
			if _, has := in["accountReference"]; has {
				t.Error("dynamic invoice must not send accountReference")
			}
			w.Write([]byte(`{"requestSuccessful":true,"responseCode":"0","responseBody":{"amount":290,"invoiceReference":"o1","invoiceStatus":"PENDING","expiryDate":"2030-01-02 13:00:00","accountNumber":"5000588061","bankName":"Moniepoint","transactionReference":"MNFY|1","checkoutUrl":"https://x"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "s", "CC")
	req := InvoiceRequest{Reference: "o1", AmountKobo: 29000, CustomerName: "A", CustomerEmail: "a@b.c",
		Description: "d", ExpiresAt: time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)}
	for i := 0; i < 2; i++ {
		inv, err := c.CreateInvoice(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if inv.AmountKobo != 29000 || inv.AccountNo != "5000588061" || inv.Status != "PENDING" {
			t.Fatalf("bad invoice: %+v", inv)
		}
	}
	if logins.Load() != 1 {
		t.Fatalf("token should be cached, logins=%d", logins.Load())
	}
}

func TestRelogsInOn401(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			n := logins.Add(1)
			w.Write([]byte(`{"requestSuccessful":true,"responseBody":{"accessToken":"t` + string(rune('0'+n)) + `","expiresIn":3600}}`))
		default:
			if r.Header.Get("Authorization") != "Bearer t2" {
				w.WriteHeader(401)
				w.Write([]byte(`{"requestSuccessful":false,"responseMessage":"expired"}`))
				return
			}
			w.Write([]byte(`{"requestSuccessful":true,"responseBody":{"invoiceReference":"o1","invoiceStatus":"PAID","amount":1.5}}`))
		}
	}))
	defer srv.Close()

	inv, err := New(srv.URL, "k", "s", "CC").GetInvoice(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status != "PAID" || inv.AmountKobo != 150 {
		t.Fatalf("bad invoice: %+v", inv)
	}
}

// Monnify sends amounts as numbers in some replies and as strings in others.
func TestGetTransactionAcceptsNumericAndStringAmounts(t *testing.T) {
	for name, amounts := range map[string]string{
		"numbers": `"amountPaid":50,"totalPayable":50`,
		"strings": `"amountPaid":"50.00","totalPayable":"50"`,
		"mixed":   `"amountPaid":"50.00","totalPayable":50.0`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auth/login" {
				w.Write([]byte(`{"requestSuccessful":true,"responseBody":{"accessToken":"t","expiresIn":3600}}`))
				return
			}
			if r.URL.EscapedPath() != "/api/v2/transactions/MNFY%7C09%7C1" {
				t.Errorf("%s: path %s", name, r.URL.EscapedPath())
			}
			w.Write([]byte(`{"requestSuccessful":true,"responseBody":{"transactionReference":"MNFY|09|1","paymentReference":"o1","paymentStatus":"PAID",` + amounts + `}}`))
		}))
		txn, err := New(srv.URL, "k", "s", "CC").GetTransaction(context.Background(), "MNFY|09|1")
		srv.Close()
		if err != nil || txn.Status != "PAID" || txn.AmountPaidKobo != 5000 || txn.TotalKobo != 5000 {
			t.Errorf("%s: %+v %v", name, txn, err)
		}
	}
}

func TestErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			w.Write([]byte(`{"requestSuccessful":true,"responseBody":{"accessToken":"t","expiresIn":3600}}`))
			return
		}
		w.WriteHeader(422)
		w.Write([]byte(`{"requestSuccessful":false,"responseMessage":"duplicate reference","responseCode":"99"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", "s", "CC").GetInvoice(context.Background(), "o1")
	e, ok := err.(*Error)
	if !ok || e.HTTPStatus != 422 || e.Code != "99" {
		t.Fatalf("want *Error 422/99, got %v", err)
	}
}

func TestDirectRoutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/auth/login":
			w.Write([]byte(`{"requestSuccessful":true,"responseBody":{"accessToken":"t","expiresIn":3600}}`))
		case r.Method == "DELETE":
			w.Write([]byte(`{"requestSuccessful":true}`))
		default:
			w.Write([]byte(`{"requestSuccessful":true,"responseBody":{"amount":250.5,"invoiceReference":"r1","invoiceStatus":"PENDING","expiryDate":"2030-01-02 13:00:00","accountNumber":"123"}}`))
		}
	}))
	defer srv.Close()

	mux := http.NewServeMux()
	New(srv.URL, "k", "s", "CC").RegisterDirectRoutes(mux)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	create := do("POST", "/monnify/invoice", `{"amount_naira":250.5,"email":"a@b.co"}`)
	var createEnv struct {
		Status string       `json:"status"`
		Data   invoiceReply `json:"data"`
	}
	json.Unmarshal(create.Body.Bytes(), &createEnv)
	if create.Code != 200 || createEnv.Status != "successful" || createEnv.Data.AmountNaira != 250.5 || createEnv.Data.AccountNo != "123" {
		t.Fatalf("create: %d %s", create.Code, create.Body)
	}
	if rec := do("GET", "/monnify/invoice/r1", ""); rec.Code != 200 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if rec := do("DELETE", "/monnify/invoice/r1", ""); rec.Code != 200 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/monnify/invoice", `{"amount_naira":0}`); rec.Code != 400 {
		t.Fatalf("missing fields accepted: %d", rec.Code)
	}
	var failEnv struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	json.Unmarshal(do("POST", "/monnify/invoice", `{"amount_naira":0}`).Body.Bytes(), &failEnv)
	if failEnv.Status != "failed" || failEnv.Error == "" {
		t.Fatalf("failed envelope: %+v", failEnv)
	}
}
