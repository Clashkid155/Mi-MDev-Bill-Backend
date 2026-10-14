// Package kora talks to Kora's merchant API for one-time bank transfer
// payments, and checks the webhooks Kora sends.
package kora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Bills-Backend/logging"
)

type Client struct {
	baseURL   string
	secretKey string
	http      *http.Client

	// Log, if set, records one line per request to Kora. Reply bodies are only
	// quoted (shortened) when a request fails.
	Log *slog.Logger
}

func New(baseURL, secretKey string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		secretKey: strings.TrimSpace(secretKey),
		http:      &http.Client{Timeout: 20 * time.Second},
	}
}

type Error struct {
	HTTPStatus int
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("kora: http %d: %s", e.HTTPStatus, e.Message)
}

// apiResponse is Kora's standard reply: status is a boolean here, not a word.
type apiResponse struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// do sends one authenticated request and decodes the data of Kora's reply into out.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
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
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

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
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !env.Status {
		msg := env.Message
		if msg == "" {
			msg = string(raw)
		}
		return &Error{HTTPStatus: resp.StatusCode, Message: msg}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// naira reads an amount that Kora may send as a number (1500, 22.5) or as a
// string ("1500.00").
type naira float64

func (n *naira) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("kora: bad amount %s", b)
	}
	*n = naira(f)
	return nil
}

func (n naira) kobo() int64 { return int64(math.Round(float64(n) * 100)) }

func (c *Client) logCall(req *http.Request, status int, reply []byte, err error, started time.Time) {
	if c.Log == nil {
		return
	}
	tag := operationName(req.Method, req.URL.Path) + " - Kora"
	took := logging.Duration(time.Since(started))
	switch {
	case err != nil:
		c.Log.Error(tag, "detail", fmt.Sprintf("no answer after %s: %v", took, err))
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
