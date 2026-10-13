package monnify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"Bills-Backend/reply"
)

// RegisterDirectRoutes exposes the Monnify calls as HTTP endpoints so the
// Monnify API can be used directly with curl or Postman. Whoever mounts these
// must protect them (see requireAdmin in main).
//
//	POST   /monnify/invoice              {"amount_naira","email","name","description","reference","expires_in_minutes"}
//	GET    /monnify/invoice/{reference}
//	DELETE /monnify/invoice/{reference}
//	GET    /monnify/transaction/{reference}   (the transaction_reference, e.g. MNFY|09|...)
//
// Only amount_naira and email are required. reference defaults to a generated
// one, expires_in_minutes to 30.
func (c *Client) RegisterDirectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /monnify/invoice", c.handleCreateInvoice)
	mux.HandleFunc("GET /monnify/invoice/{reference}", c.handleGetInvoice)
	mux.HandleFunc("DELETE /monnify/invoice/{reference}", c.handleCancelInvoice)
	mux.HandleFunc("GET /monnify/transaction/{reference}", c.handleGetTransaction)
}

type transactionReply struct {
	Reference       string  `json:"transaction_reference"`
	PaymentRef      string  `json:"payment_reference"`
	Status          string  `json:"payment_status"`
	AmountPaidNaira float64 `json:"amount_paid_naira"`
	TotalNaira      float64 `json:"total_payable_naira"`
}

func (c *Client) handleGetTransaction(w http.ResponseWriter, r *http.Request) {
	t, err := c.GetTransaction(r.Context(), r.PathValue("reference"))
	if err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply.OK(w, http.StatusOK, transactionReply{t.Reference, t.PaymentRef, t.Status,
		float64(t.AmountPaidKobo) / 100, float64(t.TotalKobo) / 100})
}

// invoiceReply is the invoice with amounts shown in naira for easy reading.
type invoiceReply struct {
	Reference    string    `json:"reference"`
	Status       string    `json:"status"`
	AmountNaira  float64   `json:"amount_naira"`
	TxnReference string    `json:"transaction_reference"`
	AccountNo    string    `json:"account_number"`
	AccountName  string    `json:"account_name"`
	BankName     string    `json:"bank_name"`
	CheckoutURL  string    `json:"checkout_url"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func replyOf(i Invoice) invoiceReply {
	return invoiceReply{i.Reference, i.Status, float64(i.AmountKobo) / 100, i.TxnReference,
		i.AccountNo, i.AccountName, i.BankName, i.CheckoutURL, i.ExpiresAt}
}

func (c *Client) handleCreateInvoice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AmountNaira      float64 `json:"amount_naira"`
		Email            string  `json:"email"`
		Name             string  `json:"name"`
		Description      string  `json:"description"`
		Reference        string  `json:"reference"`
		ExpiresInMinutes int     `json:"expires_in_minutes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.AmountNaira <= 0 || in.Email == "" {
		reply.Fail(w, http.StatusBadRequest, "amount_naira and email are required")
		return
	}
	if in.Reference == "" {
		in.Reference = fmt.Sprintf("test-%d", time.Now().UnixNano())
	}
	if in.Name == "" {
		in.Name = "Test Customer"
	}
	if in.Description == "" {
		in.Description = "Mi MDev test invoice"
	}
	if in.ExpiresInMinutes <= 0 {
		in.ExpiresInMinutes = 30
	}
	invoice, err := c.CreateInvoice(r.Context(), InvoiceRequest{
		Reference:     in.Reference,
		AmountKobo:    int64(in.AmountNaira*100 + 0.5),
		CustomerName:  in.Name,
		CustomerEmail: in.Email,
		Description:   in.Description,
		ExpiresAt:     time.Now().Add(time.Duration(in.ExpiresInMinutes) * time.Minute),
	})
	replyOrError(w, invoice, err)
}

func (c *Client) handleGetInvoice(w http.ResponseWriter, r *http.Request) {
	invoice, err := c.GetInvoice(r.Context(), r.PathValue("reference"))
	replyOrError(w, invoice, err)
}

func (c *Client) handleCancelInvoice(w http.ResponseWriter, r *http.Request) {
	if err := c.CancelInvoice(r.Context(), r.PathValue("reference")); err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply.OKWithMessage(w, http.StatusOK, "invoice cancelled", nil)
}

func replyOrError(w http.ResponseWriter, invoice Invoice, err error) {
	if err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply.OK(w, http.StatusOK, replyOf(invoice))
}
