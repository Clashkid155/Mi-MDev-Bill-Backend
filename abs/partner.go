package abs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CheckTransaction looks up an order ABS has already accepted, using the order
// id ABS gave it (the "ident" / webhook "order_id", e.g. BD2026...). ABS's own
// docs list this path under the Partner API rather than Direct, but it is
// authenticated with the same Direct API key as every other call here, no
// separate Partner credential exists in this client. It is the way to settle
// an order left PROCESSING after a timeout.
//
// ABS answers successful, Processing, failed or Refund. An error means ABS
// could not be asked or answered unreadably; that is not an order state.
func (c *Client) CheckTransaction(ctx context.Context, ident string) (Result, error) {
	status, reply, err := c.sendRequest(ctx, http.MethodGet, pathTransaction+"?ident="+url.QueryEscape(ident), nil, "")
	if err != nil {
		return Result{}, err
	}
	var body struct {
		Status  string `json:"Status"`
		Message string `json:"message"`
	}
	if status != http.StatusOK || json.Unmarshal(reply, &body) != nil {
		return Result{}, fmt.Errorf("abs: check transaction %s: http %d: %s", ident, status, shorten(reply))
	}
	result := Result{Message: body.Message, Raw: reply}
	switch strings.ToLower(body.Status) {
	case "successful":
		result.State = Success
	case "failed", "refund":
		result.State = Failed
	default:
		result.State = Processing
	}
	return result, nil
}
