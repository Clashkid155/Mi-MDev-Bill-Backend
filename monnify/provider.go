package monnify

import (
	"context"

	"Bills-Backend/payment"
)

var _ payment.Provider = (*Client)(nil)

func (c *Client) CreateCharge(ctx context.Context, r payment.ChargeRequest) (payment.Charge, error) {
	inv, err := c.CreateInvoice(ctx, InvoiceRequest{
		Reference: r.Reference, AmountKobo: r.AmountKobo, CustomerName: r.CustomerName,
		CustomerEmail: r.CustomerEmail, Description: r.Description, ExpiresAt: r.ExpiresAt,
	})
	if err != nil {
		return payment.Charge{}, err
	}
	return chargeOf(inv), nil
}

func (c *Client) ExistingCharge(ctx context.Context, reference string) (payment.Charge, error) {
	inv, err := c.GetInvoice(ctx, reference)
	if err != nil {
		return payment.Charge{}, err
	}
	return chargeOf(inv), nil
}

func chargeOf(i Invoice) payment.Charge {
	return payment.Charge{
		Reference: i.Reference, TxnRef: i.TxnReference, AccountNo: i.AccountNo, AccountName: i.AccountName,
		BankName: i.BankName, CheckoutURL: i.CheckoutURL, ExpiresAt: i.ExpiresAt,
	}
}

// Verify asks Monnify about the transaction (by Monnify's own reference).
func (c *Client) Verify(ctx context.Context, _, txnRef string) (payment.Payment, error) {
	t, err := c.GetTransaction(ctx, txnRef)
	if err != nil {
		return payment.Payment{}, err
	}
	p := payment.Payment{ReceivedKobo: t.AmountPaidKobo, Detail: t.Status}
	switch t.Status {
	case "PAID":
		p.Status = payment.Paid
	case "PARTIALLY_PAID":
		p.Status = payment.Partial
	case "OVERPAID":
		p.Status = payment.Over
	case "PENDING":
		p.Status = payment.Pending
	default: // FAILED, REVERSED, EXPIRED, ...
		p.Status = payment.Failed
	}
	return p, nil
}
