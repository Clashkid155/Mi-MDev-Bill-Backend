package monnify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"Bills-Backend/logging"
)

// Monnify expects expiry as "YYYY-MM-DD HH:MM:SS" in Nigerian time (UTC+1).
var nigerianTime = time.FixedZone("WAT", 3600)

const expiryLayout = "2006-01-02 15:04:05"

type Client struct {
	baseURL      string
	apiKey       string
	secretKey    string
	contractCode string
	http         *http.Client

	// Log, if set, records one line per request to Monnify. Reply bodies are
	// only quoted (shortened) when a request fails; login replies, which hold
	// the access token, are never quoted.
	Log *slog.Logger

	mu      sync.Mutex
	token   string
	tokenAt time.Time // when the token expires
}

func New(baseURL, apiKey, secretKey, contractCode string) *Client {
	return &Client{
		baseURL:      baseURL,
		apiKey:       apiKey,
		secretKey:    secretKey,
		contractCode: contractCode,
		http:         &http.Client{Timeout: 20 * time.Second},
	}
}

type Error struct {
	HTTPStatus int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("monnify: http %d code %s: %s", e.HTTPStatus, e.Code, e.Message)
}

type apiResponse struct {
	RequestSuccessful bool            `json:"requestSuccessful"`
	ResponseMessage   string          `json:"responseMessage"`
	ResponseCode      string          `json:"responseCode"`
	ResponseBody      json.RawMessage `json:"responseBody"`
}

// accessToken returns a cached bearer token, logging in when it is missing or
// about to expire.
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Now().Before(c.tokenAt.Add(-30*time.Second)) {
		return c.token, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+pathLogin, nil)
	if err != nil {
		return "", err
	}
	basic := base64.StdEncoding.EncodeToString([]byte(c.apiKey + ":" + c.secretKey))
	req.Header.Set("Authorization", "Basic "+basic)

	var body struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	if err := c.execute(req, &body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("monnify: login returned no access token")
	}
	c.token = body.AccessToken
	c.tokenAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return c.token, nil
}

// authedRequest sends an authenticated request, retrying once with a fresh token on 401.
func (c *Client) authedRequest(ctx context.Context, method, path string, in, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		var rd io.Reader
		if in != nil {
			b, err := json.Marshal(in)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		err = c.execute(req, out)
		if e, ok := err.(*Error); ok && e.HTTPStatus == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		return err
	}
}

// execute runs req and decodes the responseBody of Monnify's standard reply into out.
func (c *Client) execute(req *http.Request, out any) error {
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.logCall(req, 0, nil, err, started)
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c.logCall(req, resp.StatusCode, raw, err, started)
	if err != nil {
		return err
	}
	var env apiResponse
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !env.RequestSuccessful {
		msg := env.ResponseMessage
		if msg == "" {
			msg = string(raw)
		}
		return &Error{HTTPStatus: resp.StatusCode, Code: env.ResponseCode, Message: msg}
	}
	if out == nil || len(env.ResponseBody) == 0 {
		return nil
	}
	return json.Unmarshal(env.ResponseBody, out)
}

type InvoiceRequest struct {
	Reference     string // our order id; must be unique
	AmountKobo    int64
	CustomerName  string
	CustomerEmail string
	Description   string // becomes the account name on the transfer
	ExpiresAt     time.Time
}

type Invoice struct {
	Reference    string
	Status       string // PENDING, PAID, EXPIRED, ...
	AmountKobo   int64
	TxnReference string
	AccountNo    string
	AccountName  string
	BankName     string
	CheckoutURL  string
	ExpiresAt    time.Time
}

type invoiceBody struct {
	Amount               float64 `json:"amount"`
	InvoiceReference     string  `json:"invoiceReference"`
	InvoiceStatus        string  `json:"invoiceStatus"`
	ExpiryDate           string  `json:"expiryDate"`
	CheckoutURL          string  `json:"checkoutUrl"`
	AccountNumber        string  `json:"accountNumber"`
	AccountName          string  `json:"accountName"`
	BankName             string  `json:"bankName"`
	TransactionReference string  `json:"transactionReference"`
}

func (b invoiceBody) toInvoice() Invoice {
	exp, _ := time.ParseInLocation(expiryLayout, b.ExpiryDate, nigerianTime)
	return Invoice{
		Reference:    b.InvoiceReference,
		Status:       b.InvoiceStatus,
		AmountKobo:   int64(math.Round(b.Amount * 100)),
		TxnReference: b.TransactionReference,
		AccountNo:    b.AccountNumber,
		AccountName:  b.AccountName,
		BankName:     b.BankName,
		CheckoutURL:  b.CheckoutURL,
		ExpiresAt:    exp,
	}
}

// CreateInvoice creates a dynamic invoice (no accountReference), which gets a
// one-time virtual account.
func (c *Client) CreateInvoice(ctx context.Context, r InvoiceRequest) (Invoice, error) {
	in := map[string]any{
		"amount":           float64(r.AmountKobo) / 100,
		"currencyCode":     "NGN",
		"contractCode":     c.contractCode,
		"invoiceReference": r.Reference,
		"customerName":     r.CustomerName,
		"customerEmail":    r.CustomerEmail,
		"description":      r.Description,
		"expiryDate":       r.ExpiresAt.In(nigerianTime).Format(expiryLayout),
	}
	var out invoiceBody
	if err := c.authedRequest(ctx, http.MethodPost, pathCreateInvoice, in, &out); err != nil {
		return Invoice{}, err
	}
	return out.toInvoice(), nil
}

func (c *Client) GetInvoice(ctx context.Context, reference string) (Invoice, error) {
	var out invoiceBody
	if err := c.authedRequest(ctx, http.MethodGet, pathInvoiceDetails(reference), nil, &out); err != nil {
		return Invoice{}, err
	}
	return out.toInvoice(), nil
}

func (c *Client) CancelInvoice(ctx context.Context, reference string) error {
	return c.authedRequest(ctx, http.MethodDelete, pathInvoiceCancel(reference), nil, nil)
}

func (c *Client) logCall(req *http.Request, status int, reply []byte, err error, started time.Time) {
	if c.Log == nil {
		return
	}
	tag := operationName(req.URL.Path) + " - Monnify"
	took := logging.Duration(time.Since(started))
	switch {
	case err != nil:
		c.Log.Error(tag, "detail", fmt.Sprintf("no answer after %s: %v", took, err))
	case status >= 400 && req.URL.Path == pathLogin:
		c.Log.Warn(tag, "detail", fmt.Sprintf("HTTP %d · %s · login refused", status, took))
	case status >= 400:
		c.Log.Warn(tag, "detail", fmt.Sprintf("HTTP %d · %s · %s", status, took, shortReply(reply)))
	default:
		c.Log.Info(tag, "detail", fmt.Sprintf("HTTP %d · %s · %s", status, took, logging.Bytes(len(reply))))
	}
}

func shortReply(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
