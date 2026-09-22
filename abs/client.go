// Package abs is the client for ABS Vendor (https://absvendor.com/developers.php).
//
// Only the "Direct" API is used. It is form-encoded, covers airtime, data,
// electricity and vouchers, and has no reference field and no requery
// endpoint of its own (see partner.go for the one lookup ABS does offer), so a spend call must never be retried: a lost reply may still have
// charged the wallet. Outcomes that are not certain are reported as Processing.
package abs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"Bills-Backend/logging"
)

type Client struct {
	baseURL string
	apiKey  string
	sandbox bool // ABS validates everything but neither charges nor vends
	http    *http.Client

	// Log, if set, records one line per request to ABS. Reply bodies are only
	// quoted (shortened) when a request fails, never on success, because spend
	// replies can hold electricity tokens and voucher pins.
	Log *slog.Logger

	catalogMu sync.Mutex
	catalog   *Catalog
	catalogAt time.Time
}

func New(baseURL, apiKey string, sandbox bool) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		sandbox: sandbox,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type State string

const (
	Success State = "SUCCESS"
	// Failed means ABS (or our own validation) definitively rejected the order,
	// so the wallet was not charged.
	Failed State = "FAILED"
	// Processing means the outcome is unknown: timeout, 5xx or an unreadable
	// reply. Never treat it as failed.
	Processing State = "PROCESSING"
)

type Result struct {
	State   State
	Message string
	Raw     json.RawMessage // the full reply; tokens and voucher pins live in here
}

// sendRequest performs the request with the bearer key. A non-nil error means
// the outcome is unknown (network failure or timeout).
func (c *Client) sendRequest(ctx context.Context, method, path string, body io.Reader, contentType string) (status int, reply []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.logCall(method, path, 0, nil, err, started)
		return 0, nil, err
	}
	defer resp.Body.Close()
	reply, err = io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	c.logCall(method, path, resp.StatusCode, reply, err, started)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, reply, nil
}

func (c *Client) logCall(method, path string, status int, reply []byte, err error, started time.Time) {
	if c.Log == nil {
		return
	}
	tag := operationName(strings.SplitN(path, "?", 2)[0]) + " - ABS"
	took := logging.Duration(time.Since(started))
	switch {
	case err != nil:
		c.Log.Error(tag, "detail", fmt.Sprintf("no answer after %s: %v", took, err))
	case status >= 400:
		c.Log.Warn(tag, "detail", fmt.Sprintf("HTTP %d · %s · %s", status, took, shorten(reply)))
	default:
		c.Log.Info(tag, "detail", fmt.Sprintf("HTTP %d · %s · %s", status, took, logging.Bytes(len(reply))))
	}
}

const maxReplyBytes = 16 << 20

func shorten(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// Sandbox reports whether purchases are only validated, not charged or vended.
func (c *Client) Sandbox() bool { return c.sandbox }
