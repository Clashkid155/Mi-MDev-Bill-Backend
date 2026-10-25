package order

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"Bills-Backend/email"
)

// Alerter tells the owner that something needs a person. It must never slow or
// break a purchase: Alert returns at once and a failure to deliver is only logged.
//
// key names the problem (one per order for a refund, one for "ABS wallet empty"),
// and the same key is not sent again within throttle, so a run of failures is one
// email, not fifty.
type Alerter interface {
	Alert(key string, throttle time.Duration, subject, body string)
}

// EmailAlerts is an Alerter that emails the owner through the mailer the
// receipts already use. With MAIL_SANDBOX on the mailer only logs the message.
type EmailAlerts struct {
	Mailer *email.Client
	To     string
	Log    *slog.Logger

	mu   sync.Mutex
	last map[string]time.Time
	wg   sync.WaitGroup
	now  func() time.Time // replaced in tests
}

func NewEmailAlerts(mailer *email.Client, to string, log *slog.Logger) *EmailAlerts {
	return &EmailAlerts{Mailer: mailer, To: to, Log: log, last: map[string]time.Time{}, now: time.Now}
}

func (a *EmailAlerts) Alert(key string, throttle time.Duration, subject, body string) {
	a.mu.Lock()
	if at, ok := a.last[key]; ok && a.now().Sub(at) < throttle {
		a.mu.Unlock()
		return
	}
	a.last[key] = a.now()
	a.mu.Unlock()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := a.Mailer.Send(ctx, email.Message{To: a.To, ToName: "Mi MDev admin", Subject: "[Mi MDev] " + subject, Text: body})
		if err != nil {
			a.Log.Warn("Alert - Order", "detail", fmt.Sprintf("could not email the alert %q: %v", subject, err))
			// Not delivered, so let the next occurrence try again.
			a.mu.Lock()
			delete(a.last, key)
			a.mu.Unlock()
		}
	}()
}

// Wait blocks until alerts being sent have finished (call on shutdown).
func (a *EmailAlerts) Wait() { a.wg.Wait() }

// alert sends through the configured Alerter, if any.
func (s *Service) alert(key string, throttle time.Duration, subject, body string) {
	if s.Alerts != nil {
		s.Alerts.Alert(key, throttle, subject, body)
	}
}

const (
	refundThrottle  = 24 * time.Hour // one alert per order
	absEmptyEvery   = time.Hour
	feeWatchEvery   = 6 * time.Hour
	lowBalanceEvery = 6 * time.Hour
)

// isInsufficientBalance recognises ABS saying our wallet cannot pay for the
// order. ABS words it "Insufficient balance. Please fund your wallet and try
// again." under its generic failure code 141, so the words are all there is to go on.
func isInsufficientBalance(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "insufficient balance") || strings.Contains(lower, "insufficient fund")
}

// summary is the part of an alert that says which order and for how much.
func summary(o Order) string {
	what := o.Kind
	if o.PlanName != "" {
		what += ", " + o.PlanName
	}
	if o.NetworkName != "" {
		what += " (" + o.NetworkName + ")"
	}
	return fmt.Sprintf("Order: %s\nWhat: %s\nFor: %s\nCustomer: %s\nThe customer paid: %s\nPayment reference: %s\n",
		o.ID, what, o.Recipient, o.CustomerEmail, formatNaira(o.Due()), o.ID)
}

const refundSteps = "\nTo refund: find the payment in the Kora dashboard by the reference above and refund it (you can reduce the amount to cover the transaction costs).\n" +
	"Then close the order so the app stops showing it as waiting:\n  POST /admin/orders/{id}/refunded  {\"refund_fee_kobo\": ..., \"refund_amount_kobo\": ..., \"note\": \"...\"}\n"

// alertRefundDue: a customer paid and we could not deliver, so they are owed money.
func (s *Service) alertRefundDue(o Order, why string) {
	body := summary(o) + "\nWhy: " + why + "\n" + refundSteps
	if isInsufficientBalance(why) {
		body += "\nThis is the ABS wallet running out of money. Fund it before the next order, or every new order will fail the same way.\n"
	}
	s.alert("refund:"+o.ID, refundThrottle, fmt.Sprintf("Refund due: %s, order %s", formatNaira(o.Due()), o.ID), body)
	if isInsufficientBalance(why) {
		s.alert("abs-empty", absEmptyEvery, "ABS wallet is empty",
			"ABS refused an order because the wallet has too little money.\n\n"+summary(o)+"\nFund the ABS wallet now. Orders keep failing and need refunds until you do.\n")
	}
}

// alertPending: ABS gave no clear answer, so nobody knows whether the customer got what they paid for.
func (s *Service) alertPending(o Order, why string) {
	s.alert("pending:"+o.ID, refundThrottle, fmt.Sprintf("Outcome unknown: %s, order %s", formatNaira(o.Due()), o.ID),
		summary(o)+"\nABS gave no clear answer: "+why+"\n\nThe order stays PENDING. Check the transaction in the ABS dashboard (look for the recipient and amount above); "+
			"if it was delivered nothing more is needed, otherwise the customer is owed a refund.\n")
}

// WatchBalance checks the ABS wallet every `every` and alerts when it falls
// below thresholdKobo, until ctx ends. A wallet that runs out is the one failure
// that turns every order into a refund, so it is worth a warning before it happens.
func (a *API) WatchBalance(ctx context.Context, every time.Duration, thresholdKobo int64) {
	a.vending.Add(1)
	defer a.vending.Done()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		a.checkBalance(ctx, thresholdKobo)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *API) checkBalance(ctx context.Context, thresholdKobo int64) {
	kobo, err := a.Vendor.Balance(ctx)
	if err != nil {
		a.Log.Warn("WatchBalance - Order", "detail", err.Error())
		return
	}
	if kobo < thresholdKobo {
		a.alert("abs-low", lowBalanceEvery, fmt.Sprintf("ABS wallet is low: %s", formatNaira(kobo)),
			fmt.Sprintf("The ABS wallet holds %s, below the %s you asked to be warned at.\n\nFund it before it runs out; an empty wallet turns every new order into a refund.\n",
				formatNaira(kobo), formatNaira(thresholdKobo)))
	}
}
