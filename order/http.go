package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/kora"
	"Bills-Backend/monnify"
	"Bills-Backend/payment"
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
	// KoraSecret is Kora's secret key; it signs Kora's webhooks, test ones too.
	// Empty means Kora is not in use and its webhook is refused.
	KoraSecret string

	vending     sync.WaitGroup
	lastChecked sync.Map // order id -> when Monnify was last asked about it
}

const paymentCheckEvery = 5 * time.Second

// Wait blocks until in-flight purchases have finished (call on shutdown).
func (a *API) Wait() { a.vending.Wait() }

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/orders", a.createOrder)
	mux.HandleFunc("GET /v1/orders/{id}", a.getOrder)
	mux.HandleFunc("GET /v1/catalog", a.catalog)
	mux.HandleFunc("POST /v1/meters/validate", a.validateMeter)
	mux.HandleFunc("POST /webhooks/monnify", a.monnifyWebhook)
	mux.HandleFunc("POST /webhooks/kora", a.koraWebhook)
	mux.HandleFunc("POST /webhooks/abs", a.absWebhook)
	return mux
}

// AdminRoutes are for the operator, not the app: they show every customer's
// order. Whoever mounts them must protect them (see requireAdmin in main).
//
//	GET /admin/orders?status=PENDING,REFUND_DUE&limit=50
//	GET /admin/webhooks?provider=monnify&limit=20    (raw bodies as received)
//	POST /admin/orders/{id}/refunded {"refund_fee_kobo":5000,"note":"..."}  (after sending the money back by hand)
//	GET /admin/stats?from=2026-10-01&to=2026-10-20   (counts, revenue, profit, money at risk, daily series)
//	GET /admin/balances                              (what ABS and the payment provider are holding)
//
// Newest first. status is optional and may list several; limit defaults to 50 (max 200).
func (a *API) AdminRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/orders", a.listOrders)
	mux.HandleFunc("GET /admin/webhooks", a.listWebhooks)
	mux.HandleFunc("POST /admin/orders/{id}/refunded", a.markRefunded)
	mux.HandleFunc("GET /admin/stats", a.stats)
	mux.HandleFunc("GET /admin/balances", a.balances)
	return mux
}

// stats is the business at a glance, for the dashboard: counts, revenue, costs,
// profit, money at risk and a daily series. GET /admin/stats?from=2026-10-01&to=2026-10-20
// (Nigerian days, inclusive); without them it is the last 30 days.
func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	today := time.Now().In(lagos)
	from, to := today.AddDate(0, 0, -29), today
	parse := func(name string, into *time.Time) bool {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			return true
		}
		day, err := time.ParseInLocation(dateLayout, raw, lagos)
		if err != nil {
			reply.Fail(w, http.StatusBadRequest, name+" must be a day like 2026-10-20")
			return false
		}
		*into = day
		return true
	}
	if !parse("from", &from) || !parse("to", &to) {
		return
	}
	if to.Before(from) {
		reply.Fail(w, http.StatusBadRequest, "to must not be before from")
		return
	}
	if to.Sub(from) > 366*24*time.Hour {
		reply.Fail(w, http.StatusBadRequest, "ask for at most a year at a time")
		return
	}
	st, err := a.Store.Stats(r.Context(), from, to)
	if err != nil {
		a.Log.Error("Stats - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	reply.OK(w, http.StatusOK, st)
}

// A balancer is a payment provider that can say how much it is holding.
type balancer interface {
	Balance(ctx context.Context) (payment.Balance, error)
}

// balances shows what each account is holding. A provider that fails to answer
// shows its error and does not hide the others.
func (a *API) balances(w http.ResponseWriter, r *http.Request) {
	type account struct {
		AvailableKobo *int64 `json:"available_kobo"`
		PendingKobo   *int64 `json:"pending_kobo,omitempty"`
		Error         string `json:"error,omitempty"`
	}
	out := map[string]account{}

	if kobo, err := a.Vendor.Balance(r.Context()); err != nil {
		a.Log.Warn("Balances - API", "detail", err.Error())
		out["abs"] = account{Error: err.Error()}
	} else {
		out["abs"] = account{AvailableKobo: &kobo}
	}

	name := "payments"
	if b, ok := a.Pay.(balancer); ok {
		if bal, err := b.Balance(r.Context()); err != nil {
			a.Log.Warn("Balances - API", "detail", "payments: "+err.Error())
			out[name] = account{Error: err.Error()}
		} else {
			out[name] = account{AvailableKobo: &bal.AvailableKobo, PendingKobo: &bal.PendingKobo}
		}
	} else {
		out[name] = account{Error: "this payment provider cannot report a balance here"}
	}
	reply.OK(w, http.StatusOK, out)
}

// listWebhooks shows the webhooks exactly as received, newest first.
func (a *API) listWebhooks(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			reply.Fail(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	events, err := a.Store.Webhooks(r.Context(), r.URL.Query().Get("provider"), limit)
	if err != nil {
		a.Log.Error("ListWebhooks - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	type view struct {
		ID         int64     `json:"id"`
		Provider   string    `json:"provider"`
		EventKey   string    `json:"event_key"`
		ReceivedAt time.Time `json:"received_at"`
		Body       any       `json:"body"`
	}
	views := make([]view, 0, len(events))
	for _, e := range events {
		var body any = e.Body
		if json.Valid([]byte(e.Body)) {
			body = json.RawMessage(e.Body)
		}
		views = append(views, view{e.ID, e.Provider, e.EventKey, e.ReceivedAt, body})
	}
	reply.OK(w, http.StatusOK, map[string]any{"count": len(views), "webhooks": views})
}

type adminOrderView struct {
	ID          string `json:"id"`
	Status      Status `json:"status"`
	Kind        string `json:"kind"`
	Network     string `json:"network,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
	PlanID      string `json:"plan_id,omitempty"`
	PlanName    string `json:"plan_name,omitempty"`
	PlanNote    string `json:"plan_note,omitempty"`
	Recipient   string `json:"recipient"`
	AmountKobo  int64  `json:"amount_kobo"`
	FeeKobo     int64  `json:"fee_kobo"`
	TotalKobo   int64  `json:"total_kobo"`
	CostKobo    int64  `json:"cost_kobo"`
	PaymentFee  int64  `json:"payment_fee_kobo"`
	// Set once a REFUND_DUE order is marked refunded.
	RefundFee  int64      `json:"refund_fee_kobo,omitempty"`
	RefundNote string     `json:"refund_note,omitempty"`
	RefundedAt *time.Time `json:"refunded_at,omitempty"`
	// ProfitKobo is what the order earned: only known for a SUCCESS order whose
	// cost was kept (orders made before costs were stored show null).
	ProfitKobo    *int64    `json:"profit_kobo"`
	CustomerEmail string    `json:"customer_email"`
	TxnReference  string    `json:"transaction_reference,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func refundedAt(o Order) *time.Time {
	if o.RefundedAt.IsZero() {
		return nil
	}
	return &o.RefundedAt
}

func adminViewOf(o Order) adminOrderView {
	return adminOrderView{ID: o.ID, Status: o.Status, Kind: o.Kind, Network: o.Network, NetworkName: o.NetworkName,
		PlanID: o.PlanID, PlanName: o.PlanName, PlanNote: o.PlanNote, Recipient: o.Recipient, AmountKobo: o.AmountKobo, FeeKobo: o.FeeKobo,
		TotalKobo: o.Due(), CostKobo: o.CostKobo, PaymentFee: o.PaymentFeeKobo, ProfitKobo: o.Profit(),
		RefundFee: o.RefundFeeKobo, RefundNote: o.RefundNote, RefundedAt: refundedAt(o),
		CustomerEmail: o.CustomerEmail, TxnReference: o.TxnRef, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
}

// markRefunded closes an order in REFUND_DUE after the money was sent back by
// hand. It does not move any money itself. The body is optional:
//
//	{"refund_fee_kobo": 5000, "note": "refunded from the Kora dashboard", "payment_fee_returned": false}
//
// refund_fee_kobo is what sending the refund cost; it goes into the net profit.
// payment_fee_returned says the provider gave its fee on the original payment back.
func (a *API) markRefunded(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefundFeeKobo int64  `json:"refund_fee_kobo"`
		Note          string `json:"note"`
		// PaymentFeeReturned: the payment provider gave back its fee on the original payment.
		PaymentFeeReturned bool `json:"payment_fee_returned"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		reply.Fail(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 && json.Unmarshal(body, &req) != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if req.RefundFeeKobo < 0 {
		reply.Fail(w, http.StatusBadRequest, "refund_fee_kobo cannot be negative")
		return
	}
	if len([]rune(req.Note)) > 500 {
		reply.Fail(w, http.StatusBadRequest, "note must be at most 500 characters")
		return
	}
	id := r.PathValue("id")
	if current, err := a.Store.ByID(r.Context(), id); err == nil && req.RefundFeeKobo > current.Due() {
		reply.Fail(w, http.StatusBadRequest, "refund_fee_kobo is more than the order")
		return
	}
	o, err := a.Store.MarkRefunded(r.Context(), id, req.RefundFeeKobo, req.Note, req.PaymentFeeReturned)
	switch {
	case errors.Is(err, ErrNotFound):
		reply.Fail(w, http.StatusNotFound, "order not found")
	case errors.Is(err, ErrNotRefundable):
		reply.Fail(w, http.StatusConflict, "only an order in REFUND_DUE can be marked refunded; this one is "+string(o.Status))
	case err != nil:
		a.Log.Error("MarkRefunded - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
	default:
		a.Log.Info("MarkRefunded - API", "detail", fmt.Sprintf("order %s marked refunded (the refund cost %d kobo)", o.ID, o.RefundFeeKobo))
		reply.OK(w, http.StatusOK, adminViewOf(o))
	}
}

func (a *API) listOrders(w http.ResponseWriter, r *http.Request) {
	var statuses []Status
	if raw := r.URL.Query().Get("status"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			st := Status(strings.ToUpper(strings.TrimSpace(name)))
			if !slices.Contains(allStatuses, st) {
				reply.Fail(w, http.StatusBadRequest, fmt.Sprintf("unknown status %q", name))
				return
			}
			statuses = append(statuses, st)
		}
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			reply.Fail(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	orders, err := a.Store.List(r.Context(), statuses, limit)
	if err != nil {
		a.Log.Error("ListOrders - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	views := make([]adminOrderView, 0, len(orders))
	for _, o := range orders {
		views = append(views, adminViewOf(o))
	}
	reply.OK(w, http.StatusOK, map[string]any{"count": len(views), "orders": views})
}

// feeRule is the electricity service fee: the greater of MinKobo and Percent
// of the amount, rounded up to whole naira.
type feeRule struct {
	MinKobo int64   `json:"min_kobo"`
	Percent float64 `json:"percent"`
}

type orderView struct {
	ID        string `json:"id"`
	Status    Status `json:"status"`
	Kind      string `json:"kind"`
	Network   string `json:"network,omitempty"`
	PlanID    string `json:"plan_id,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	// Note is the plan's own warning as it was when the order was made (a fair
	// use limit, "won't credit if the line owes airtime"). Show it before the
	// customer pays. Absent when the plan has none.
	Note string `json:"note,omitempty"`
	// AmountKobo is the value bought; FeeKobo a service fee on top of it
	// (electricity); TotalKobo what the customer pays, which is what the account
	// number below is waiting for.
	AmountKobo int64        `json:"amount_kobo"`
	FeeKobo    int64        `json:"fee_kobo"`
	TotalKobo  int64        `json:"total_kobo"`
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
		Recipient: o.Recipient, Note: o.PlanNote, AmountKobo: o.AmountKobo, FeeKobo: o.FeeKobo, TotalKobo: o.Due(), Delivery: deliveryOf(o)}
	// An expired order's account number is dead; do not offer it for payment.
	if o.AccountNo != "" && o.Status != StatusExpired {
		v.Payment = &paymentView{o.AccountNo, o.AccountName, o.BankName, o.CheckoutURL, o.ExpiresAt}
	}
	return v
}

// deliveryOf returns the "data" block of ABS's reply for electricity, where it
// is the product itself (the token), once the order succeeded.
func deliveryOf(o Order) json.RawMessage {
	if o.Status != StatusSuccess {
		return nil
	}
	return deliveryData(o.Kind, json.RawMessage(o.VendResponse))
}

// deliveryData extracts ABS's "data" block from a raw vend reply. For
// electricity that block is the token itself, the thing the customer paid for.
func deliveryData(kind string, raw json.RawMessage) json.RawMessage {
	if kind != KindElectricity || len(raw) == 0 {
		return nil
	}
	var parsed struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &parsed) != nil || len(parsed.Data) == 0 || string(parsed.Data) == "null" {
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
		if o.Status == StatusAwaitingPayment || o.Status == StatusPartiallyPaid {
			o = a.checkPayment(r.Context(), o)
		} else {
			a.lastChecked.Delete(o.ID)
		}
		reply.OK(w, http.StatusOK, viewOf(o))
	}
}

// checkPayment asks Monnify whether a waiting order has been paid, so an order
// still moves on when the webhook is late or never arrives. Polling apps would
// otherwise hit Monnify on every request, so each order is checked at most
// once per paymentCheckEvery. Returns the order as it is afterwards.
func (a *API) checkPayment(ctx context.Context, o Order) Order {
	if last, ok := a.lastChecked.Load(o.ID); ok && time.Since(last.(time.Time)) < paymentCheckEvery {
		return o
	}
	a.lastChecked.Store(o.ID, time.Now())
	paid, err := a.Settle(ctx, o)
	if err != nil {
		a.Log.Warn("CheckPayment - API", "detail", fmt.Sprintf("order %s: %v", o.ID, err))
		return o
	}
	if paid != nil {
		a.vendInBackground(*paid)
	}
	if fresh, err := a.Store.ByID(ctx, o.ID); err == nil {
		return fresh
	}
	return o
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
	catalog, err := a.SellCatalog(r.Context())
	if err != nil {
		a.Log.Error("Catalog - API", "detail", err.Error())
		reply.Fail(w, http.StatusBadGateway, "catalog unavailable")
		return
	}
	reply.OK(w, http.StatusOK, struct {
		abs.Catalog
		// ElectricityFee tells the app how the service fee on electricity is worked
		// out, so it can show the total before the customer orders. The order
		// itself carries the exact fee.
		ElectricityFee feeRule `json:"electricity_fee"`
	}{catalog, feeRule{MinKobo: a.Pricing.ElectricityFeeMinKobo, Percent: a.Pricing.ElectricityFeePercent}})
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
			PaymentReference     string `json:"paymentReference"` // our order id
		} `json:"eventData"`
	}
	if json.Unmarshal(body, &event) != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := a.Store.LogWebhook(r.Context(), "monnify", event.EventData.TransactionReference, body); err != nil {
		a.Log.Error("Webhook - API", "detail", "could not save webhook: "+err.Error())
	}
	// SUCCESSFUL_TRANSACTION is a complete or over payment; TRANSACTION is how Monnify
	// reports a partial one. Either way Monnify is asked what really happened.
	if event.EventType != "SUCCESSFUL_TRANSACTION" && event.EventType != "TRANSACTION" {
		// Saved above for reference; nothing to act on. Acknowledged so Monnify does not retry.
		a.Log.Info("Webhook - Monnify", "detail", fmt.Sprintf("ignoring %s event (order %s)", event.EventType, event.EventData.PaymentReference))
		reply.OK(w, http.StatusOK, nil)
		return
	}
	o, err := a.ConfirmPayment(r.Context(), event.EventData.PaymentReference, event.EventData.TransactionReference)
	if err != nil {
		a.Log.Error("MonnifyWebhook - API", "detail", "could not confirm payment: "+err.Error())
		reply.Fail(w, http.StatusInternalServerError, "could not confirm payment") // Monnify will retry
		return
	}
	if o != nil {
		a.vendInBackground(*o)
	}
	reply.OK(w, http.StatusOK, nil)
}

// koraWebhook handles Kora's charge.success and charge.failed events. Kora signs
// the data object with the secret key and retries for up to 72 hours until it
// gets a 200, so the same event can arrive many times; ConfirmPayment ignores
// an order that has already moved on.
func (a *API) koraWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		reply.Fail(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if a.KoraSecret == "" {
		reply.Fail(w, http.StatusServiceUnavailable, "kora is not configured")
		return
	}
	if err := kora.VerifyWebhook(a.KoraSecret, r.Header, body); err != nil {
		a.Log.Warn("Webhook - Kora", "detail", "rejected: "+err.Error())
		reply.Fail(w, http.StatusUnauthorized, "bad signature")
		return
	}
	var event struct {
		Event string `json:"event"`
		Data  struct {
			Reference        string `json:"reference"` // our order id
			PaymentReference string `json:"payment_reference"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &event) != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := a.Store.LogWebhook(r.Context(), "kora", event.Data.Reference, body); err != nil {
		a.Log.Error("Webhook - API", "detail", "could not save webhook: "+err.Error())
	}
	if event.Event != "charge.success" {
		a.Log.Info("Webhook - Kora", "detail", fmt.Sprintf("ignoring %s event (order %s)", event.Event, event.Data.Reference))
		reply.OK(w, http.StatusOK, nil)
		return
	}
	// Kora is asked what really happened; the webhook's amount is only ever what we requested.
	o, err := a.ConfirmPayment(r.Context(), event.Data.Reference, event.Data.PaymentReference)
	if err != nil {
		a.Log.Error("KoraWebhook - API", "detail", "could not confirm payment: "+err.Error())
		reply.Fail(w, http.StatusInternalServerError, "could not confirm payment") // Kora will retry
		return
	}
	if o != nil {
		a.vendInBackground(*o)
	}
	reply.OK(w, http.StatusOK, nil)
}

// Sweep expires unpaid orders whose payment window has passed, once at the
// start and then every `every`, until ctx ends. Polling only settles orders the
// app still asks about; this covers the ones nobody does (app closed, phone off),
// and settles a payment whose webhook never arrived.
func (a *API) Sweep(ctx context.Context, every time.Duration) {
	a.vending.Add(1)
	defer a.vending.Done()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		a.sweepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *API) sweepOnce(ctx context.Context) {
	orders, err := a.Store.AwaitingPastExpiry(ctx, time.Now(), 100)
	if err != nil {
		a.Log.Error("Sweep - Order", "detail", err.Error())
		return
	}
	for _, o := range orders {
		paid, err := a.Settle(ctx, o)
		if err != nil {
			a.Log.Warn("Sweep - Order", "detail", fmt.Sprintf("order %s: %v", o.ID, err))
			continue
		}
		if paid != nil {
			a.vendInBackground(*paid)
		}
	}
}

// vendInBackground buys from ABS without holding the caller up: Monnify's
// webhook times out, and the app should not wait on ABS while polling.
func (a *API) vendInBackground(o Order) {
	a.vending.Add(1)
	go func() {
		defer a.vending.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := a.Vend(ctx, o); err != nil {
			a.Log.Error("Vend - Order", "detail", fmt.Sprintf("order %s: %v", o.ID, err))
		}
	}()
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
