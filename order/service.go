package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/monnify"
)

const invoiceLifetime = 30 * time.Minute

type Service struct {
	Store  *Store
	Pay    *monnify.Client
	Vendor *abs.Client
	Log    *slog.Logger
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
			CustomerEmail: r.Email, ExpiresAt: time.Now().Add(invoiceLifetime),
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
		CustomerName:  strings.SplitN(o.CustomerEmail, "@", 2)[0],
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

// ConfirmPayment is called when Monnify says an invoice was paid. It does not
// trust the webhook: it asks Monnify for the invoice and checks it is PAID for
// the right amount. It returns the order (now PAID) if this call is the one
// that should vend it, or nil if there is nothing to do (unknown order,
// already handled, or not really paid).
func (s *Service) ConfirmPayment(ctx context.Context, txnRef string) (*Order, error) {
	o, err := s.Store.ByTxnRef(ctx, txnRef)
	if errors.Is(err, ErrNotFound) {
		s.Log.Warn("ConfirmPayment - Order", "detail", fmt.Sprintf("payment received for unknown transaction %s", txnRef))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if o.Status != StatusAwaitingPayment && o.Status != StatusExpired {
		return nil, nil // duplicate webhook
	}

	invoice, err := s.Pay.GetInvoice(ctx, o.InvoiceRef)
	if err != nil {
		return nil, err // let the webhook be retried
	}
	if invoice.Status != "PAID" || invoice.AmountKobo != o.AmountKobo {
		// TODO: under/over-payment needs a refund record; for now it is only logged.
		s.Log.Warn("ConfirmPayment - Order", "detail", fmt.Sprintf("order %s: invoice is %s, paid %d kobo, expected %d kobo",
			o.ID, invoice.Status, invoice.AmountKobo, o.AmountKobo))
		return nil, nil
	}
	moved, err := s.Store.Move(ctx, o.ID, o.Status, StatusPaid)
	if err != nil || !moved {
		return nil, err
	}
	o.Status = StatusPaid
	return &o, nil
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
		_, err = s.Store.MoveWithVendResponse(ctx, o.ID, StatusVending, StatusSuccess, raw)
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

func logVend(log *slog.Logger, orderID string, result abs.Result) {
	detail := fmt.Sprintf("order %s: %s · %s", orderID, result.State, result.Message)
	if result.State == abs.Success {
		log.Info("Vend - Order", "detail", detail)
	} else {
		log.Warn("Vend - Order", "detail", detail)
	}
}
