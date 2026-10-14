package kora

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"Bills-Backend/payment"
	"Bills-Backend/reply"
)

// RegisterDirectRoutes exposes the Kora calls as HTTP endpoints so the Kora API
// can be tried with curl or Postman. Whoever mounts these must protect them
// (see requireAdmin in main), and they are only mounted while no real money is
// in use.
//
//	POST /kora/charge             {"amount_naira","email","name","reference"}
//	GET  /kora/charge/{reference}  asks Kora what happened to the charge
//
// Only amount_naira and email are required; reference defaults to a generated one.
func (c *Client) RegisterDirectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /kora/charge", c.handleCreateCharge)
	mux.HandleFunc("GET /kora/charge/{reference}", c.handleGetCharge)
}

func (c *Client) handleCreateCharge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AmountNaira float64 `json:"amount_naira"`
		Email       string  `json:"email"`
		Name        string  `json:"name"`
		Reference   string  `json:"reference"`
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
	charge, err := c.CreateCharge(r.Context(), payment.ChargeRequest{
		Reference: in.Reference, AmountKobo: int64(in.AmountNaira*100 + 0.5),
		CustomerName: in.Name, CustomerEmail: in.Email, Description: "Mi MDev test charge",
	})
	if err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply.OK(w, http.StatusOK, map[string]any{
		"reference": charge.Reference, "account_number": charge.AccountNo, "account_name": charge.AccountName,
		"bank_name": charge.BankName, "expires_at": charge.ExpiresAt,
	})
}

func (c *Client) handleGetCharge(w http.ResponseWriter, r *http.Request) {
	p, err := c.Verify(r.Context(), r.PathValue("reference"), "")
	if err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply.OK(w, http.StatusOK, map[string]any{
		"status": p.Status, "kora_status": p.Detail, "amount_accepted_naira": float64(p.ReceivedKobo) / 100,
	})
}
