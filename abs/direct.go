package abs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The Direct API answers every call with this shape:
//
//	success: {"status": true,  "text": "Successful", "data": {...}}
//	failure: {"status": false, "text": "...", "error": {"code": 141, "text": "..."}}
type directResponse struct {
	Status *bool           `json:"status"`
	Text   string          `json:"text"`
	Data   json.RawMessage `json:"data"`
	// Error is kept raw: its shape is only known for failures, and a success
	// reply may carry it as null, [] or something else. Reading it must never
	// make a good reply unparseable.
	Error json.RawMessage `json:"error"`
}

// errorDetail returns ABS's error code and text if Error is an object.
func (r directResponse) errorDetail() (code int, text string) {
	var detail struct {
		Code int    `json:"code"`
		Text string `json:"text"`
	}
	if json.Unmarshal(r.Error, &detail) != nil {
		return 0, ""
	}
	if r.Text != "" {
		return detail.Code, r.Text
	}
	return detail.Code, detail.Text
}

func (r directResponse) failureReason(httpStatus int, reply []byte) string {
	code, text := r.errorDetail()
	switch {
	case text != "":
		return fmt.Sprintf("code %d: %s", code, text)
	case r.Text != "":
		return r.Text
	}
	return fmt.Sprintf("http %d: %s", httpStatus, shorten(reply))
}

// Airtime buys airtime. network is an airtime product trackid such as
// MTNAIR6TFD (see SystemData). Minimum ₦50, whole naira only.
func (c *Client) Airtime(ctx context.Context, network, phone string, amountKobo int64) Result {
	naira, ok := wholeNaira(amountKobo)
	if !ok {
		return rejected("amount must be a positive whole number of naira")
	}
	return c.postSpend(ctx, pathBuyAirtime, url.Values{
		"airtimetid": {network},
		"phoneno":    {phone},
		"amount":     {strconv.FormatInt(naira, 10)},
	})
}

// Data buys a data plan. network is a data network trackid such as MTN56TFD
// and plan a plan trackid such as 6BO1H. Both come from SystemData and differ
// from the airtime ids (mixing them gives error code 141).
func (c *Client) Data(ctx context.Context, network, plan, phone string) Result {
	return c.postSpend(ctx, pathBuyData, url.Values{
		"airtimetid":  {network},
		"cardunittid": {plan},
		"phoneno":     {phone},
	})
}

// Electricity tops up a meter. disco is a service trackid from SystemData.
// Minimum ₦1000, whole naira only. Call ValidateMeter first.
func (c *Client) Electricity(ctx context.Context, disco, meter string, amountKobo int64) Result {
	naira, ok := wholeNaira(amountKobo)
	if !ok {
		return rejected("amount must be a positive whole number of naira")
	}
	return c.postSpend(ctx, pathBuyElectric, url.Values{
		"provider_tid": {disco},
		"meter_number": {meter},
		"amount":       {strconv.FormatInt(naira, 10)},
	})
}

// Voucher buys 1-30 airtime vouchers. unit is a voucher unit trackid such as
// HBSI2 (MTN ₦500).
func (c *Client) Voucher(ctx context.Context, network, unit string, quantity int) Result {
	if quantity < 1 || quantity > 30 {
		return rejected("voucher quantity must be 1-30")
	}
	return c.postSpend(ctx, pathBuyVoucher, url.Values{
		"airtimetid":  {network},
		"cardunittid": {unit},
		"totalunit":   {strconv.Itoa(quantity)},
	})
}

// Rejected means ABS answered and refused the request (as opposed to being
// unreachable or answering with something unreadable).
type Rejected struct{ Reason string }

func (e Rejected) Error() string { return "abs: rejected: " + e.Reason }

// ValidateMeter is free and read-only. It returns ABS's data block, which
// holds the meter owner's name and address. A meter ABS does not accept comes
// back as a Rejected error.
func (c *Client) ValidateMeter(ctx context.Context, disco, meter string) (json.RawMessage, error) {
	form := url.Values{"provider_tid": {disco}, "meter_number": {meter}}
	status, reply, err := c.sendRequest(ctx, http.MethodPost, pathValidateMeter,
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return nil, err
	}
	var resp directResponse
	_ = json.Unmarshal(reply, &resp)
	refused := resp.Status != nil && !*resp.Status || status >= 400 && status < 500
	switch {
	case refused:
		return nil, Rejected{Reason: resp.failureReason(status, reply)}
	case status != http.StatusOK || resp.Status == nil:
		return nil, fmt.Errorf("abs: validate meter: %s", resp.failureReason(status, reply))
	}
	return resp.Data, nil
}

// SystemData returns the full live catalog: airtime providers, data plans,
// electricity services and voucher providers. Only the airtimeproviders shape
// is documented, so the raw JSON is returned.
func (c *Client) SystemData(ctx context.Context) (json.RawMessage, error) {
	status, reply, err := c.sendRequest(ctx, http.MethodGet, pathSystemData, nil, "")
	if err != nil {
		return nil, err
	}
	var resp directResponse
	if status != http.StatusOK {
		return nil, fmt.Errorf("abs: system data: http %d: %s", status, shorten(reply))
	}
	if err := json.Unmarshal(reply, &resp); err != nil {
		return nil, fmt.Errorf("abs: system data: could not read the reply (%d bytes): %w", len(reply), err)
	}
	return resp.Data, nil
}

// postSpend sends a form-encoded spend call and classifies the outcome.
func (c *Client) postSpend(ctx context.Context, path string, form url.Values) Result {
	if c.sandbox {
		form.Set("sandbox", "1")
	}
	status, reply, err := c.sendRequest(ctx, http.MethodPost, path,
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return Result{State: Processing, Message: err.Error()}
	}
	var resp directResponse
	parsed := json.Unmarshal(reply, &resp) == nil && resp.Status != nil
	result := Result{Raw: reply}
	switch {
	case status == http.StatusOK && parsed && *resp.Status:
		result.State, result.Message = Success, resp.Text
	case parsed && !*resp.Status, status >= 400 && status < 500:
		result.State, result.Message = Failed, resp.failureReason(status, reply)
	default:
		result.State, result.Message = Processing, resp.failureReason(status, reply)
	}
	if c.Log != nil && result.State != Success {
		c.Log.Warn(operationName(path)+" - ABS", "detail", fmt.Sprintf("%s · %s", result.State, result.Message))
	}
	return result
}

// rejected is for orders we refuse to send. Nothing reached ABS, so it is a
// definite failure.
func rejected(reason string) Result {
	return Result{State: Failed, Message: reason}
}

func wholeNaira(kobo int64) (naira int64, ok bool) {
	if kobo <= 0 || kobo%100 != 0 {
		return 0, false
	}
	return kobo / 100, true
}
