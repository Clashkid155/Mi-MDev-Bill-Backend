package abs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Balance is the naira in the ABS wallet. It comes from the account's profile
// reply, which also carries personal details (name, phone, email, bank account,
// login history): only the wallet is read, and the reply is never logged.
func (c *Client) Balance(ctx context.Context) (kobo int64, err error) {
	status, reply, err := c.sendRequest(ctx, http.MethodGet, pathUserDetail, nil, "")
	if err != nil {
		return 0, err
	}
	var resp struct {
		Status *bool `json:"status"`
		Data   struct {
			Wallets []struct {
				Name        string `json:"name"`
				CurrencyTag string `json:"currencytag"`
				Balance     string `json:"walletbal"`
			} `json:"user_wallets"`
		} `json:"data"`
	}
	if status != http.StatusOK || json.Unmarshal(reply, &resp) != nil || resp.Status == nil || !*resp.Status {
		return 0, fmt.Errorf("abs: could not read the wallet balance (http %d)", status)
	}
	for _, w := range resp.Data.Wallets {
		if w.Name == "NGN" {
			if kobo, ok := nairaToKobo(w.Balance); ok {
				return kobo, nil
			}
			return 0, fmt.Errorf("abs: unreadable wallet balance %q", w.Balance)
		}
	}
	return 0, fmt.Errorf("abs: the profile has no naira wallet")
}
