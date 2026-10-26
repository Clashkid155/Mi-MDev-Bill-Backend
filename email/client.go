// Package email sends transactional email through ZeptoMail
// (https://www.zoho.com/cpaas/help/api/email-sending.html).
//
// One HTTP call per message to POST /v1.1/email, authorised with a "send mail
// token" (the Authorization header value is "Zoho-enczapikey <token>"). The
// account and mail agent are derived from the token.
//
// ZeptoMail has no sandbox mode of its own, so the sandbox flag here is
// local: when it is on, Send logs and returns without contacting ZeptoMail at
// all. It is on by default (like ABS's) so no real mail goes out until it is
// deliberately turned off.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"Bills-Backend/logging"
)

// Settings is everything the client needs. Only BaseURL, Token and FromEmail
// are required for real sending.
type Settings struct {
	BaseURL   string // e.g. https://cpaas.zoho.com, depends on the account's data centre
	Token     string // the send mail token; a pasted "Zoho-enczapikey " prefix is accepted
	FromEmail string // must belong to a domain verified in ZeptoMail
	FromName  string
	// BounceAddress is optional. Some ZeptoMail accounts require one; leave it
	// empty unless ZeptoMail asks for it.
	BounceAddress string
	// Sandbox skips the network call entirely (see the package comment).
	Sandbox bool
}

type Client struct {
	baseURL       string
	token         string
	fromEmail     string
	fromName      string
	bounceAddress string
	sandbox       atomic.Bool // can be switched while the server runs (SetSandbox)
	http          *http.Client

	// Log, if set, records one line per request to ZeptoMail. Reply bodies are
	// only quoted (shortened) on failure, never on success.
	Log *slog.Logger
}

func New(s Settings) *Client {
	c := &Client{
		baseURL:       strings.TrimRight(s.BaseURL, "/"),
		token:         strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.Token), "Zoho-enczapikey")),
		fromEmail:     s.FromEmail,
		fromName:      s.FromName,
		bounceAddress: s.BounceAddress,
		http:          &http.Client{Timeout: 15 * time.Second},
	}
	c.sandbox.Store(s.Sandbox)
	return c
}

// Sandbox reports whether messages are logged instead of sent.
func (c *Client) Sandbox() bool { return c.sandbox.Load() }

// SetSandbox switches between logging messages (true) and sending them (false).
func (c *Client) SetSandbox(on bool) { c.sandbox.Store(on) }

// Message is one email to one recipient. Text or HTML (or both) is required.
type Message struct {
	To      string
	ToName  string
	Subject string
	Text    string
	HTML    string
}

type party struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

type recipient struct {
	EmailAddress party `json:"email_address"`
}

type sendBody struct {
	BounceAddress string      `json:"bounce_address,omitempty"`
	From          party       `json:"from"`
	To            []recipient `json:"to"`
	Subject       string      `json:"subject"`
	TextBody      string      `json:"textbody,omitempty"`
	HTMLBody      string      `json:"htmlbody,omitempty"`
}

// failureReply is ZeptoMail's error shape:
//
//	{"data": {"error_code": "TM_3004", "message": "Invalid request"}, "message": "error"}
//
// On success "data" is an array instead, so it is kept raw and only read for failures.
type failureReply struct {
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

func (r failureReply) reason() string {
	var detail struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(r.Data, &detail) == nil && detail.Message != "" {
		if detail.ErrorCode != "" {
			return detail.ErrorCode + ": " + detail.Message
		}
		return detail.Message
	}
	return r.Message
}

// Send sends one message. A non-nil error means ZeptoMail refused or could
// not be reached; the caller decides whether that should block anything (it
// should not block an order, since email is a courtesy, not the product).
func (c *Client) Send(ctx context.Context, m Message) error {
	if m.Text == "" && m.HTML == "" {
		return fmt.Errorf("email: message has no body")
	}
	if c.Sandbox() {
		if c.Log != nil {
			c.Log.Info(operationName(pathSend)+" - ZeptoMail", "detail", "sandbox, not sent")
		}
		return nil
	}

	raw, err := json.Marshal(sendBody{
		BounceAddress: c.bounceAddress,
		From:          party{Address: c.fromEmail, Name: c.fromName},
		To:            []recipient{{EmailAddress: party{Address: m.To, Name: m.ToName}}},
		Subject:       m.Subject,
		TextBody:      m.Text,
		HTMLBody:      m.HTML,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+pathSend, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Zoho-enczapikey "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.logCall(0, nil, err, started)
		return err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c.logCall(resp.StatusCode, reply, err, started)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var f failureReply
		if json.Unmarshal(reply, &f) == nil && f.reason() != "" {
			return fmt.Errorf("email: zeptomail rejected the message (http %d): %s", resp.StatusCode, f.reason())
		}
		return fmt.Errorf("email: http %d: %s", resp.StatusCode, shorten(reply))
	}
	return nil
}

func (c *Client) logCall(status int, reply []byte, err error, started time.Time) {
	if c.Log == nil {
		return
	}
	tag := operationName(pathSend) + " - ZeptoMail"
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

func shorten(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
