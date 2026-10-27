package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/email"
	"Bills-Backend/payment"
)

const invoiceLifetime = 30 * time.Minute

type Service struct {
	Store  *Store
	Pay    payment.Provider
	Vendor *abs.Client
	Log    *slog.Logger
	// Mailer sends the purchase-successful email. Nil disables it (no email
	// provider configured); this is optional, unlike Pay and Vendor.
	Mailer *email.Client
	// Alerts tells the owner when something needs a person: a refund is due, an
	// order's outcome is unknown, the ABS wallet is empty, the payment fee is too
	// high. Nil sends nothing (the log still records it).
	Alerts Alerter
	// MinPaymentKobo is the smallest payment the payment provider accepts
	// (Kora: 10000, i.e. ₦100). Airtime below it cannot be ordered and data
	// plans priced below it are refused and hidden from the catalog. 0 means no
	// minimum beyond ABS's own.
	MinPaymentKobo int64
	// Settings, when set, holds the pricing rules and the fee warning limit and lets
	// the admin change them while the server runs; they are read from it on every
	// order, so a change applies to the next one. When nil, Pricing and
	// FeeWarnPercent below are used as they are.
	Settings *Settings
	// Pricing marks up what ABS charges, hides some plans and sets the
	// electricity service fee. The zero value changes nothing.
	Pricing Pricing
	// FeeWarnPercent: log a warning when the payment provider's fee on a payment
	// is more than this percent of it (the fee we planned prices around is about
	// 1.6%). 0 turns the watch off.
	FeeWarnPercent float64

	creating keyedLock // one Create at a time per idempotency key
}

// keyedLock is a mutex per key, for work that must not run twice at once for the
// same key. The zero value is ready to use. It only works inside one process,
// which is all this server ever is (SQLite allows one).
type keyedLock struct {
	mu    sync.Mutex
	locks map[string]*heldLock
}

type heldLock struct {
	sync.Mutex
	users int
}

// lock waits for key and returns the function that lets the next caller in.
func (k *keyedLock) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*heldLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &heldLock{}
		k.locks[key] = l
	}
	l.users++
	k.mu.Unlock()

	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// SellCatalog is the catalog the customer is shown and orders from: ABS's own,
// with the pricing rules applied and anything below the payment provider's
// minimum left out. Prices always come from here, never from the app.
func (s *Service) SellCatalog(ctx context.Context) (abs.Catalog, error) {
	catalog, err := s.Vendor.Catalog(ctx)
	if err != nil {
		return abs.Catalog{}, err
	}
	return withinMinimumPayment(s.pricing().Apply(catalog), s.MinPaymentKobo), nil
}

// pricing is the pricing rules in force right now.
func (s *Service) pricing() Pricing {
	if s.Settings != nil {
		return s.Settings.Pricing()
	}
	return s.Pricing
}

// feeWarnPercent is the payment fee limit above which the owner is warned.
func (s *Service) feeWarnPercent() float64 {
	if s.Settings != nil {
		return s.Settings.Float("payment_fee_warn_percent")
	}
	return s.FeeWarnPercent
}

// customerName is the name given for the customer, or the part of their email
// before the @ when none was given (the payment provider insists on a name).
func customerName(given, email string) string {
	if name := strings.TrimSpace(given); name != "" {
		return name
	}
	return strings.SplitN(email, "@", 2)[0]
}

// Create starts a purchase: it records the order and asks the payment provider for a
// one-time account the customer pays into. Sending the same idempotency key
// again returns the same order.
func (s *Service) Create(ctx context.Context, r CreateRequest) (Order, error) {
	r.Email = strings.TrimSpace(r.Email)
	if err := r.validate(); err != nil {
		return Order{}, err
	}
	// The app retries a request whose answer it did not see, sometimes while the
	// first is still running. Taking turns per key makes the second one find the
	// first one's order instead of failing on the duplicate key or asking the
	// payment provider for a second account.
	defer s.creating.lock(r.IdempotencyKey)()
	o, err := s.Store.ByIdempotencyKey(ctx, r.IdempotencyKey)
	if errors.Is(err, ErrNotFound) {
		var p product
		if p, err = s.resolve(ctx, r); err != nil {
			return Order{}, err
		}
		o = Order{
			IdempotencyKey: r.IdempotencyKey, Kind: r.Kind, Network: p.Network, PlanID: p.PlanID,
			Recipient: p.Recipient, AmountKobo: p.AmountKobo, CostKobo: p.CostKobo, FeeKobo: p.FeeKobo, TotalKobo: p.AmountKobo + p.FeeKobo,
			CustomerEmail: r.Email, CustomerName: customerName(r.CustomerName, r.Email),
			NetworkName: p.NetworkName, PlanName: p.PlanName, PlanNote: p.PlanNote,
			ExpiresAt: time.Now().Add(invoiceLifetime),
		}
		err = s.Store.Insert(ctx, &o)
	}
	if err != nil {
		return Order{}, err
	}
	if o.Status != StatusInitiated {
		return o, nil // already has its invoice (or moved on)
	}

	invoice, err := s.Pay.CreateCharge(ctx, payment.ChargeRequest{
		Reference:     o.ID,
		AmountKobo:    o.Due(),
		CustomerName:  o.CustomerName,
		CustomerEmail: o.CustomerEmail,
		Description:   "Mi MDev " + o.Kind,
		ExpiresAt:     o.ExpiresAt,
	})
	if err != nil {
		// A previous attempt may have created the charge before we lost the reply.
		existing, lookupErr := s.Pay.ExistingCharge(ctx, o.ID)
		if lookupErr != nil {
			return Order{}, fmt.Errorf("create invoice: %w", err)
		}
		invoice = existing
	}
	o.InvoiceRef, o.TxnRef = invoice.Reference, invoice.TxnRef
	o.AccountNo, o.AccountName, o.BankName, o.CheckoutURL = invoice.AccountNo, invoice.AccountName, invoice.BankName, invoice.CheckoutURL
	if !invoice.ExpiresAt.IsZero() {
		o.ExpiresAt = invoice.ExpiresAt
	}
	if err := s.Store.AttachInvoice(ctx, &o); err != nil {
		return Order{}, err
	}
	return o, nil
}

// ConfirmPayment is called when the provider says a payment came in, or when we
// check on an order ourselves. paymentRef is our own order id and txnRef is the
// provider's reference for the charge; either can be empty, and the order is
// found by whichever is given.
//
// It does not trust the webhook: it asks the provider what happened and checks
// the payment is settled for exactly the amount due. It returns the order (now
// PAID) if this call is the one that should vend it, or nil if there is
// nothing to do (unknown order, already handled, or not really paid).
//
// An order that already expired is never vended. If money turns up for one
// anyway it is marked REFUND_DUE.
func (s *Service) ConfirmPayment(ctx context.Context, paymentRef, txnRef string) (*Order, error) {
	o, err := s.orderFor(ctx, paymentRef, txnRef)
	if errors.Is(err, ErrNotFound) {
		s.Log.Warn("ConfirmPayment - Order", "detail", fmt.Sprintf("payment received for unknown order (reference %q, transaction %q)", paymentRef, txnRef))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if o.Status != StatusAwaitingPayment && o.Status != StatusPartiallyPaid && o.Status != StatusExpired {
		return nil, nil // duplicate webhook, or already dealt with
	}
	if o.TxnRef != "" {
		txnRef = o.TxnRef
	}
	if txnRef == "" {
		return nil, nil // no invoice yet, so nothing can have been paid
	}

	txn, err := s.Pay.Verify(ctx, o.ID, txnRef)
	if err != nil {
		return nil, err // let the webhook be retried
	}
	if txn.Status != payment.Paid || txn.ReceivedKobo != o.Due() {
		// Not paid, or paid the wrong amount. Nothing is vended. A wrong amount is
		// recorded as what it is, PARTIALLY_PAID or OVERPAID, and the order is
		// never expired afterwards, since the customer did pay something.
		if txn.Status != payment.Pending {
			s.Log.Warn("ConfirmPayment - Order", "detail", fmt.Sprintf("order %s: payment is %s, received %d kobo, expected %d kobo",
				o.ID, txn.Detail, txn.ReceivedKobo, o.Due()))
		}
		if to, ok := wrongAmountStatus(txn, o.Due()); ok && o.Status != StatusExpired && o.Status != to {
			if _, err := s.Store.Move(ctx, o.ID, o.Status, to); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if o.Status == StatusExpired {
		if _, err := s.Store.Move(ctx, o.ID, StatusExpired, StatusRefundDue); err != nil {
			return nil, err
		}
		s.Log.Warn("ConfirmPayment - Order", "detail", fmt.Sprintf("order %s: %d kobo arrived after it expired, refund needed", o.ID, txn.ReceivedKobo))
		s.alertRefundDue(o, "the payment arrived after the order had expired, so nothing was bought")
		return nil, nil
	}
	moved, err := s.Store.Move(ctx, o.ID, o.Status, StatusPaid)
	if err != nil || !moved {
		return nil, err
	}
	o.Status = StatusPaid
	s.recordPaymentFee(ctx, o, txn)
	return &o, nil
}

// recordPaymentFee keeps what the payment provider took (for profit figures)
// and warns when it is more than planned for: prices are worked out for a fee
// of about 1.6% and a provider that quietly raises it turns sales into losses.
// It never fails the payment.
func (s *Service) recordPaymentFee(ctx context.Context, o Order, txn payment.Payment) {
	if txn.FeeKobo <= 0 {
		return
	}
	if err := s.Store.SetPaymentFee(ctx, o.ID, txn.FeeKobo); err != nil {
		s.Log.Warn("PaymentFee - Order", "detail", fmt.Sprintf("order %s: could not save the payment fee: %v", o.ID, err))
	}
	// Compared in whole numbers: 170 of 10,000 is exactly 1.7%, not 1.7000000000000002.
	warn := s.feeWarnPercent()
	if limit := percentMilli(warn); limit > 0 && txn.ReceivedKobo > 0 {
		if txn.FeeKobo*100_000 > txn.ReceivedKobo*limit {
			percent := float64(txn.FeeKobo) / float64(txn.ReceivedKobo) * 100
			s.Log.Warn("PaymentFee - Order", "detail", fmt.Sprintf(
				"order %s: the payment provider took %.2f%% (%d kobo of %d), above the %.2f%% prices were worked out for; check the fee agreement",
				o.ID, percent, txn.FeeKobo, txn.ReceivedKobo, warn))
			s.alert("fee-watch", feeWatchEvery, fmt.Sprintf("Payment fee is %.2f%%, above the %.2f%% prices were built for", percent, warn),
				fmt.Sprintf("Order %s: the payment provider took %s of a %s payment (%.2f%%).\n\nPrices assume about 1.61%%. If this is the new rate, the markups may need raising; "+
					"check the fee in the provider's dashboard.\n", o.ID, formatNaira(txn.FeeKobo), formatNaira(txn.ReceivedKobo), percent))
		}
	}
}

// wrongAmountStatus says which status a payment of the wrong amount puts an
// order in. ok is false when the provider reports something else (pending,
// failed, reversed), which is not money we are holding.
func wrongAmountStatus(txn payment.Payment, due int64) (status Status, ok bool) {
	switch {
	case txn.Status == payment.Partial, txn.Status == payment.Paid && txn.ReceivedKobo < due:
		return StatusPartiallyPaid, true
	case txn.Status == payment.Over, txn.Status == payment.Paid && txn.ReceivedKobo > due:
		return StatusOverpaid, true
	}
	return "", false
}

func (s *Service) orderFor(ctx context.Context, paymentRef, txnRef string) (Order, error) {
	if paymentRef != "" {
		if o, err := s.Store.ByID(ctx, paymentRef); !errors.Is(err, ErrNotFound) {
			return o, err
		}
	}
	return s.Store.ByTxnRef(ctx, txnRef)
}

// Settle checks an order that is still waiting for payment. If Monnify says it
// was paid it returns the order (now PAID) to be vended. A wrong amount is
// recorded by ConfirmPayment as PARTIALLY_PAID or OVERPAID and is left alone
// here: the customer did pay something, so the order is never expired.
// Only once the window has passed with nothing paid at all does the order
// become EXPIRED. Monnify is asked first, so a payment made just before the
// deadline is not lost to a slow webhook.
func (s *Service) Settle(ctx context.Context, o Order) (*Order, error) {
	paid, err := s.ConfirmPayment(ctx, o.ID, o.TxnRef)
	if err != nil || paid != nil {
		return paid, err
	}
	if time.Now().After(o.ExpiresAt) {
		moved, err := s.Store.Move(ctx, o.ID, StatusAwaitingPayment, StatusExpired)
		if err != nil {
			return nil, err
		}
		if moved {
			s.Log.Info("Expire - Order", "detail", fmt.Sprintf("order %s: not paid by %s, expired", o.ID, o.ExpiresAt.Format("15:04:05")))
		}
	}
	return nil, nil
}

// Vend buys the product from ABS for a PAID order and records the outcome.
// It is safe to call once per order: only the caller that wins the
// PAID -> VENDING move contacts ABS.
func (s *Service) Vend(ctx context.Context, o Order) error {
	moved, err := s.Store.Move(ctx, o.ID, StatusPaid, StatusVending)
	if err != nil || !moved {
		return err
	}
	result := s.buy(ctx, o)
	logVend(s.Log, o.ID, result)

	raw := string(result.Raw)
	switch result.State {
	case abs.Success:
		if _, err = s.Store.MoveWithVendResponse(ctx, o.ID, StatusVending, StatusSuccess, raw); err == nil {
			s.sendReceipt(ctx, o, result.Raw)
		}
	case abs.Failed:
		if _, err = s.Store.MoveWithVendResponse(ctx, o.ID, StatusVending, StatusFailed, raw); err == nil {
			if _, err = s.Store.Move(ctx, o.ID, StatusFailed, StatusRefundDue); err == nil {
				s.alertRefundDue(o, result.Message)
			}
		}
	default:
		// Outcome unknown. Stays PENDING until ABS's webhook or a manual check settles it.
		if _, err = s.Store.MoveWithVendResponse(ctx, o.ID, StatusVending, StatusPending, raw); err == nil {
			s.alertPending(o, result.Message)
		}
	}
	return err
}

// sendReceipt emails the customer once their order has actually succeeded.
// It never fails the order: a broken or unconfigured mailer only gets logged.
func (s *Service) sendReceipt(ctx context.Context, o Order, vendResponse json.RawMessage) {
	if s.Mailer == nil || o.CustomerEmail == "" {
		return
	}
	r := buildReceipt(o, deliveryData(o.Kind, vendResponse), time.Now())
	page, err := r.html()
	if err != nil {
		// The plain version still goes out; the customer should not lose their receipt over a template bug.
		s.Log.Warn("SendReceipt - Order", "detail", fmt.Sprintf("order %s: html template: %v", o.ID, err))
	}
	msg := email.Message{
		To: o.CustomerEmail, ToName: o.CustomerName,
		Subject: fmt.Sprintf("Your Mi MDev %s purchase was successful", o.Kind),
		Text:    r.text(), HTML: page,
	}
	if err := s.Mailer.Send(ctx, msg); err != nil {
		s.Log.Warn("SendReceipt - Order", "detail", fmt.Sprintf("order %s: %v", o.ID, err))
	}
}

func logVend(log *slog.Logger, orderID string, result abs.Result) {
	detail := fmt.Sprintf("order %s: %s · %s", orderID, result.State, result.Message)
	if result.State == abs.Success {
		log.Info("Vend - Order", "detail", detail)
	} else {
		log.Warn("Vend - Order", "detail", detail)
	}
}
