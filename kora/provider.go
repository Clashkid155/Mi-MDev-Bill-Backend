package kora

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"Bills-Backend/payment"
)

var _ payment.Provider = (*Client)(nil)

// accountName is what the customer's banking app shows when they look up the
// account number to pay.
const accountName = "Mi MDev"

// CreateCharge opens a one-time Kora account for the order. Kora chooses the
// expiry itself, so r.ExpiresAt is not sent; the expiry Kora returns is used.
//
// merchant_bears_cost is true: the customer pays exactly the price shown, and
// the fee comes out of what we settle. With false Kora would ask the customer
// for price plus fee, which the app does not show.
func (c *Client) CreateCharge(ctx context.Context, r payment.ChargeRequest) (payment.Charge, error) {
	in := map[string]any{
		"reference":           r.Reference,
		"amount":              float64(r.AmountKobo) / 100,
		"currency":            "NGN",
		"narration":           r.Description,
		"account_name":        accountName,
		"merchant_bears_cost": true,
		"customer":            map[string]string{"name": r.CustomerName, "email": r.CustomerEmail},
	}
	var out struct {
		Reference      string `json:"reference"`
		Narration      string `json:"narration"`
		Amount         naira  `json:"amount"`
		AmountExpected naira  `json:"amount_expected"`
		BankAccount    struct {
			AccountName string `json:"account_name"`
			AccountNo   string `json:"account_number"`
			BankName    string `json:"bank_name"`
			Expires     string `json:"expiry_date_in_utc"`
		} `json:"bank_account"`
	}
	if err := c.do(ctx, http.MethodPost, pathBankTransfer, in, &out); err != nil {
		return payment.Charge{}, err
	}
	// Kora's dashboard has shown its own "Payment on <merchant>" text instead of
	// the narration we send; record what it kept so that can be told apart from a bug.
	if c.Log != nil && out.Narration != r.Description {
		c.Log.Info("CreateCharge - Kora", "detail", fmt.Sprintf("sent narration %q, Kora kept %q", r.Description, out.Narration))
	}
	// Never show the customer an account that wants a different amount than the price.
	if out.AmountExpected != 0 && out.AmountExpected.kobo() != r.AmountKobo {
		return payment.Charge{}, fmt.Errorf("kora: account expects %d kobo but the price is %d kobo", out.AmountExpected.kobo(), r.AmountKobo)
	}
	if out.BankAccount.AccountNo == "" {
		return payment.Charge{}, fmt.Errorf("kora: reply has no account number")
	}
	exp, _ := time.Parse(time.RFC3339Nano, out.BankAccount.Expires)
	return payment.Charge{
		Reference: r.Reference, TxnRef: r.Reference, // Kora is asked about our own reference
		AccountNo: out.BankAccount.AccountNo, AccountName: out.BankAccount.AccountName,
		BankName: out.BankAccount.BankName, ExpiresAt: exp,
	}, nil
}

// ExistingCharge cannot get the account back: Kora's query reply is about the
// payment, not the account to pay into.
func (c *Client) ExistingCharge(context.Context, string) (payment.Charge, error) {
	return payment.Charge{}, payment.ErrNotRecoverable
}

// Verify is Kora's Transaction Query. The amount that counts is amount_accepted,
// not amount (always what we asked for) or amount_paid (what arrived, which Kora
// may have returned in part).
func (c *Client) Verify(ctx context.Context, reference, _ string) (payment.Payment, error) {
	var out struct {
		Status         string `json:"status"`
		AmountPaid     naira  `json:"amount_paid"`
		AmountAccepted *naira `json:"amount_accepted"`
		Fee            naira  `json:"fee"`
	}
	if err := c.do(ctx, http.MethodGet, pathCharge(reference), nil, &out); err != nil {
		return payment.Payment{}, err
	}
	p := payment.Payment{Detail: out.Status, FeeKobo: out.Fee.kobo()}
	if out.AmountAccepted != nil {
		p.ReceivedKobo = out.AmountAccepted.kobo()
	} else {
		p.ReceivedKobo = out.AmountPaid.kobo()
	}
	switch out.Status {
	case "success":
		p.Status = payment.Paid
	case "failed":
		p.Status = payment.Failed
	default: // processing, pending, or anything new: not money we can act on
		p.Status = payment.Pending
	}
	return p, nil
}
