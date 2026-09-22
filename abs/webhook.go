package abs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var ErrBadSignature = errors.New("abs: invalid webhook signature")

const webhookTolerance = 5 * time.Minute

func webhookSignature(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// VerifyWebhook checks X-ABS-Signature (HMAC-SHA256 over timestamp + "." + raw
// body) and rejects timestamps more than 5 minutes from now.
func VerifyWebhook(secret string, h http.Header, body []byte, now time.Time) error {
	ts := h.Get("X-ABS-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > webhookTolerance || d < -webhookTolerance {
		return ErrBadSignature
	}
	got := strings.ToLower(strings.TrimSpace(h.Get("X-ABS-Signature")))
	if !hmac.Equal([]byte(got), []byte(webhookSignature(secret, ts, body))) {
		return ErrBadSignature
	}
	return nil
}

type Event struct {
	Name        string // transaction.settled, transaction.failed, refund.issued, wallet.*
	DeliveryID  string // X-ABS-Delivery: stable across retries, dedupe on this
	OrderID     string
	Status      string // "1" settled, "3" failed, "6" refunded
	Amount      string
	Recipient   string
	Message     string
	ConfirmedAt string // events can arrive out of order: newest confirmed_at wins
}

func ParseWebhook(h http.Header, body []byte) (Event, error) {
	var b struct {
		Event string `json:"event"`
		Data  struct {
			OrderID     string `json:"order_id"`
			Status      string `json:"status"`
			Amount      string `json:"amount"`
			Recipient   string `json:"recipient"`
			Message     string `json:"message"`
			ConfirmedAt string `json:"confirmed_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return Event{}, err
	}
	return Event{
		Name: b.Event, DeliveryID: h.Get("X-ABS-Delivery"),
		OrderID: b.Data.OrderID, Status: b.Data.Status, Amount: b.Data.Amount,
		Recipient: b.Data.Recipient, Message: b.Data.Message, ConfirmedAt: b.Data.ConfirmedAt,
	}, nil
}
