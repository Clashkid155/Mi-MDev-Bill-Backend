package kora

import (
	"context"
	"fmt"
	"net/http"

	"Bills-Backend/payment"
)

// Balance is the naira Kora is holding for us. The path (GET /api/v1/balances)
// and the reply shape ({"data": {"NGN": {"available_balance", "pending_balance"}}})
// are from memory and have not been tried against Kora yet: if the first call
// fails, the error says so and nothing else is affected.
func (c *Client) Balance(ctx context.Context) (payment.Balance, error) {
	var out map[string]struct {
		Available naira `json:"available_balance"`
		Pending   naira `json:"pending_balance"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/balances", nil, &out); err != nil {
		return payment.Balance{}, err
	}
	ngn, ok := out["NGN"]
	if !ok {
		return payment.Balance{}, fmt.Errorf("kora: the balances reply has no NGN balance")
	}
	return payment.Balance{AvailableKobo: ngn.Available.kobo(), PendingKobo: ngn.Pending.kobo()}, nil
}
