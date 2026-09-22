package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/monnify"
	"Bills-Backend/reply"
)

const maxBody = 1 << 20

// API serves the order endpoints and the provider webhooks.
type API struct {
	*Service

	MonnifySecret string
	// VerifyMonnifySignature must be true in production. Monnify does not sign
	// sandbox webhooks; ConfirmPayment still re-checks every payment with Monnify.
	VerifyMonnifySignature bool
	ABSWebhookSecret       string // empty skips signature checks (sandbox only)

	vending sync.WaitGroup
}

// Wait blocks until in-flight purchases have finished (call on shutdown).
func (a *API) Wait() { a.vending.Wait() }

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/orders", a.createOrder)
	mux.HandleFunc("GET /v1/orders/{id}", a.getOrder)
	mux.HandleFunc("GET /v1/catalog", a.catalog)
	mux.HandleFunc("POST /v1/meters/validate", a.validateMeter)
	mux.HandleFunc("POST /webhooks/monnify", a.monnifyWebhook)
	mux.HandleFunc("POST /webhooks/abs", a.absWebhook)
	return mux
}

type orderView struct {
	ID         string       `json:"id"`
	Status     Status       `json:"status"`
	Kind       string       `json:"kind"`
	Network    string       `json:"network,omitempty"`
	PlanID     string       `json:"plan_id,omitempty"`
	Recipient  string       `json:"recipient,omitempty"`
	AmountKobo int64        `json:"amount_kobo"`
	Payment    *paymentView `json:"payment,omitempty"`
	// Delivery holds what ABS handed back for an electricity token, once the
	// order has succeeded. It is the thing the customer paid for.
	Delivery json.RawMessage `json:"delivery,omitempty"`
}

type paymentView struct {
	AccountNumber string    `json:"account_number"`
	AccountName   string    `json:"account_name"`
	BankName      string    `json:"bank_name"`
	CheckoutURL   string    `json:"checkout_url"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func viewOf(o Order) orderView {
	v := orderView{ID: o.ID, Status: o.Status, Kind: o.Kind, Network: o.Network, PlanID: o.PlanID,
		Recipient: o.Recipient, AmountKobo: o.AmountKobo, Delivery: deliveryOf(o)}
	if o.AccountNo != "" {
		v.Payment = &paymentView{o.AccountNo, o.AccountName, o.BankName, o.CheckoutURL, o.ExpiresAt}
	}
	return v
}

// deliveryOf returns the "data" block of ABS's reply for electricity, where it
// is the product itself (the token), once the order succeeded.
func deliveryOf(o Order) json.RawMessage {
	if o.Status != StatusSuccess || o.Kind != KindElectricity {
		return nil
	}
	var parsed struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(o.VendResponse), &parsed) != nil || len(parsed.Data) == 0 || string(parsed.Data) == "null" {
		return nil
	}
	return parsed.Data
}

func (a *API) createOrder(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	o, err := a.Create(r.Context(), req)
	var invalid InvalidError
	switch {
	case errors.As(err, &invalid):
		reply.Fail(w, http.StatusBadRequest, invalid.Message)
	case err != nil:
		a.Log.Error("CreateOrder - API", "detail", err.Error())
		reply.Fail(w, http.StatusBadGateway, "could not start the payment, try again")
	default:
		reply.OK(w, http.StatusOK, viewOf(o))
	}
}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request) {
	o, err := a.Store.ByID(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		reply.Fail(w, http.StatusNotFound, "order not found")
	case err != nil:
		a.Log.Error("GetOrder - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
	default:
		reply.OK(w, http.StatusOK, viewOf(o))
	}
}

// validateMeter lets the app show the meter's owner before the customer pays.
func (a *API) validateMeter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServiceID string `json:"service_id"`
		Meter     string `json:"meter"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	details, err := a.Service.ValidateMeter(r.Context(), req.ServiceID, strings.TrimSpace(req.Meter))
	var invalid InvalidError
	switch {
	case errors.As(err, &invalid):
		reply.Fail(w, http.StatusBadRequest, invalid.Message)
	case err != nil:
		a.Log.Error("ValidateMeter - API", "detail", err.Error())
		reply.Fail(w, http.StatusBadGateway, "could not check this meter, try again")
	default:
		reply.OK(w, http.StatusOK, details)
	}
}

// catalog is what the app shows: only purchasable things, only needed fields.
func (a *API) catalog(w http.ResponseWriter, r *http.Request) {
	catalog, err := a.Vendor.Catalog(r.Context())
	if err != nil {
		a.Log.Error("Catalog - API", "detail", err.Error())
		reply.Fail(w, http.StatusBadGateway, "catalog unavailable")
		return
	}
	reply.OK(w, http.StatusOK, catalog)
}

func (a *API) monnifyWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		reply.Fail(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if a.VerifyMonnifySignature {
		if err := monnify.VerifyWebhook(a.MonnifySecret, r.Header, body); err != nil {
			reply.Fail(w, http.StatusUnauthorized, "bad signature")
			return
		}
	}
	var event struct {
		EventType string `json:"eventType"`
		EventData struct {
			TransactionReference string `json:"transactionReference"`
		} `json:"eventData"`
	}
	if json.Unmarshal(body, &event) != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := a.Store.LogWebhook(r.Context(), "monnify", event.EventData.TransactionReference, body); err != nil {
		a.Log.Error("Webhook - API", "detail", "could not save webhook: "+err.Error())
	}
	if event.EventType != "SUCCESSFUL_TRANSACTION" {
		reply.OK(w, http.StatusOK, nil)
		return
	}
	o, err := a.ConfirmPayment(r.Context(), event.EventData.TransactionReference)
	if err != nil {
		a.Log.Error("MonnifyWebhook - API", "detail", "could not confirm payment: "+err.Error())
		reply.Fail(w, http.StatusInternalServerError, "could not confirm payment") // Monnify will retry
		return
	}
	if o != nil {
		// Reply now; buying from ABS can take a while and Monnify times out.
		a.vending.Add(1)
		go func() {
			defer a.vending.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := a.Vend(ctx, *o); err != nil {
				a.Log.Error("Vend - Order", "detail", fmt.Sprintf("order %s: %v", o.ID, err))
			}
		}()
	}
	reply.OK(w, http.StatusOK, nil)
}

// absWebhook only records ABS events for now. ABS's Direct API returns no
// order id, so matching an event to an order is not designed yet.
func (a *API) absWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		reply.Fail(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if a.ABSWebhookSecret != "" {
		if err := abs.VerifyWebhook(a.ABSWebhookSecret, r.Header, body, time.Now()); err != nil {
			reply.Fail(w, http.StatusUnauthorized, "bad signature")
			return
		}
	}
	if err := a.Store.LogWebhook(r.Context(), "abs", r.Header.Get("X-ABS-Delivery"), body); err != nil {
		a.Log.Error("Webhook - API", "detail", "could not save webhook: "+err.Error())
		reply.Fail(w, http.StatusInternalServerError, "could not save webhook")
		return
	}
	reply.OK(w, http.StatusOK, nil)
}
