package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/email"
	"Bills-Backend/monnify"
)

const invoiceLifetime = 30 * time.Minute

type Service struct {
	Store  *Store
	Pay    *monnify.Client
	Vendor *abs.Client
	Log    *slog.Logger
	// Mailer sends the purchase-successful email. Nil disables it (no email
	// provider configured); this is optional, unlike Pay and Vendor.
	Mailer *email.Client
	// MinPaymentKobo is the smallest payment the payment provider accepts
	// (Kora: 10000, i.e. ₦100). Airtime below it cannot be ordered and data
	// plans priced below it are refused and hidden from the catalog. 0 means no
	// minimum beyond ABS's own.
	MinPaymentKobo int64
}

// customerName is the name given for the customer, or the part of their email
// before the @ when none was given (Monnify insists on a name).
func customerName(given, email string) string {
	if name := strings.TrimSpace(given); name != "" {
		return name
	}
	return strings.SplitN(email, "@", 2)[0]
}

// Create starts a purchase: it records the order and asks Monnify for a
// one-time account the customer pays into. Sending the same idempotency key
// again returns the same order.
func (s *Service) Create(ctx context.Context, r CreateRequest) (Order, error) {
	if err := r.validate(); err != nil {
		return Order{}, err
	}
	o, err := s.Store.ByIdempotencyKey(ctx, r.IdempotencyKey)
	if errors.Is(err, ErrNotFound) {
		var p product
		if p, err = s.resolve(ctx, r); err != nil {
			return Order{}, err
		}
		o = Order{
			IdempotencyKey: r.IdempotencyKey, Kind: r.Kind, Network: p.Network, PlanID: p.PlanID,
			Recipient: p.Recipient, AmountKobo: p.AmountKobo,
			CustomerEmail: r.Email, CustomerName: customerName(r.CustomerName, r.Email),
			NetworkName: p.NetworkName, PlanName: p.PlanName,
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

	invoice, err := s.Pay.CreateInvoice(ctx, monnify.InvoiceRequest{
		Reference:     o.ID,
		AmountKobo:    o.AmountKobo,
		CustomerName:  o.CustomerName,
		CustomerEmail: o.CustomerEmail,
		Description:   "Mi MDev " + o.Kind,
		ExpiresAt:     o.ExpiresAt,
	})
	if err != nil {
		// A previous attempt may have created the invoice before we lost the reply.
		existing, lookupErr := s.Pay.GetInvoice(ctx, o.ID)
		if lookupErr != nil {
			return Order{}, fmt.Errorf("create invoice: %w", err)
		}
		invoice = existing
	}
	o.InvoiceRef, o.TxnRef = invoice.Reference, invoice.TxnReference
	o.AccountNo, o.AccountName, o.BankName, o.CheckoutURL = invoice.AccountNo, invoice.AccountName, invoice.BankName, invoice.CheckoutURL
	if !invoice.ExpiresAt.IsZero() {
		o.ExpiresAt = invoice.ExpiresAt
	}
	if err := s.Store.AttachInvoice(ctx, &o); err != nil {
		return Order{}, err
	}
	return o, nil
}

// ConfirmPayment is called when Monnify says a payment came in, or when we
// check on an order ourselves. paymentRef is our own order id (Monnify's
// paymentReference) and txnRef is Monnify's transactionReference; either can
// be empty, and the order is found by whichever is given.
//
// It does not trust the webhook: it asks Monnify for the transaction and
// checks it is PAID for exactly the amount due. It returns the order (now
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

	txn, err := s.Pay.GetTransaction(ctx, txnRef)
	if err != nil {
		return nil, err // let the webhook be retried
	}
	if txn.Status != "PAID" || txn.AmountPaidKobo != o.AmountKobo {
		// Not paid, or paid the wrong amount. Nothing is vended. A wrong amount is
		// recorded as what it is, PARTIALLY_PAID or OVERPAID, and the order is
		// never expired afterwards, since the customer did pay something.
		if txn.Status != "PENDING" {
			s.Log.Warn("ConfirmPayment - Order", "detail", fmt.Sprintf("order %s: transaction is %s, paid %d kobo, expected %d kobo",
				o.ID, txn.Status, txn.AmountPaidKobo, o.AmountKobo))
		}
		if to, ok := wrongAmountStatus(txn, o.AmountKobo); ok && o.Status != StatusExpired && o.Status != to {
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
		s.Log.Warn("ConfirmPayment - Order", "detail", fmt.Sprintf("order %s: %d kobo arrived after it expired, refund needed", o.ID, txn.AmountPaidKobo))
		return nil, nil
	}
	moved, err := s.Store.Move(ctx, o.ID, o.Status, StatusPaid)
	if err != nil || !moved {
		return nil, err
	}
	o.Status = StatusPaid
	return &o, nil
}

// wrongAmountStatus says which status a payment of the wrong amount puts an
// order in. ok is false when Monnify reports something else (pending, failed,
// reversed), which is not money we are holding.
func wrongAmountStatus(txn monnify.Transaction, due int64) (status Status, ok bool) {
	switch {
	case txn.Status == "PARTIALLY_PAID", txn.Status == "PAID" && txn.AmountPaidKobo < due:
		return StatusPartiallyPaid, true
	case txn.Status == "OVERPAID", txn.Status == "PAID" && txn.AmountPaidKobo > due:
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
			_, err = s.Store.Move(ctx, o.ID, StatusFailed, StatusRefundDue)
		}
	default:
		// Outcome unknown. Stays PENDING until ABS's webhook or a manual check settles it.
		_, err = s.Store.MoveWithVendResponse(ctx, o.ID, StatusVending, StatusPending, raw)
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
