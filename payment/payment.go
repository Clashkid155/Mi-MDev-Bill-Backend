// Package payment is what the order flow needs from a payment provider, so the
// provider (Monnify, Kora) can change without touching the order code.
package payment

import (
	"context"
	"errors"
	"time"
)

// ChargeRequest asks for a one-time bank account the customer pays into.
type ChargeRequest struct {
	Reference     string // our order id; unique, at least 8 characters
	AmountKobo    int64
	CustomerName  string
	CustomerEmail string
	Description   string
	ExpiresAt     time.Time // a provider that chooses its own expiry ignores this
}

// Charge is the account the customer pays into.
type Charge struct {
	Reference   string
	TxnRef      string // the provider's own reference; equals Reference if it has none
	AccountNo   string
	AccountName string
	BankName    string
	CheckoutURL string
	ExpiresAt   time.Time // zero if the provider did not say
}

// Status is a payment's outcome, whatever words the provider uses for it.
type Status string

const (
	Pending Status = "PENDING" // nothing usable has arrived yet
	Paid    Status = "PAID"    // settled; ReceivedKobo says how much counts
	Partial Status = "PARTIAL" // the provider reports less than was due
	Over    Status = "OVER"    // the provider reports more than was due
	Failed  Status = "FAILED"  // failed, reversed or cancelled
)

// Payment is what the provider says about one charge.
type Payment struct {
	Status Status
	// ReceivedKobo is the amount we may give value for: what Monnify says was
	// paid, or Kora's amount_accepted.
	ReceivedKobo int64
	// Detail is the provider's own status text, for logs.
	Detail string
}

// ErrNotRecoverable means the provider cannot hand back the account of a charge
// that was created earlier, so a lost reply cannot be recovered by asking again.
var ErrNotRecoverable = errors.New("payment: provider cannot return an existing charge")

// Provider is implemented by the monnify and kora packages.
type Provider interface {
	// CreateCharge opens a one-time account for r.
	CreateCharge(ctx context.Context, r ChargeRequest) (Charge, error)
	// ExistingCharge finds a charge created earlier for reference, for when the
	// reply to CreateCharge was lost.
	ExistingCharge(ctx context.Context, reference string) (Charge, error)
	// Verify asks the provider what happened to a charge. reference is our order
	// id and txnRef the Charge.TxnRef stored on the order; a provider uses the
	// one it knows.
	Verify(ctx context.Context, reference, txnRef string) (Payment, error)
}
