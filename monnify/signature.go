package monnify

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

var ErrBadSignature = errors.New("monnify: invalid webhook signature")

// Signature returns hex(HMAC-SHA512(secret, rawBody)), which Monnify sends in
// the monnify-signature header on production webhooks.
func Signature(secret string, rawBody []byte) string {
	m := hmac.New(sha512.New, []byte(secret))
	m.Write(rawBody)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyWebhook checks the header against the raw body. Sandbox webhooks are
// unsigned, so callers may skip verification only when running in sandbox and
// must then confirm the invoice via GetInvoice before acting.
func VerifyWebhook(secret string, h http.Header, rawBody []byte) error {
	got := strings.ToLower(strings.TrimSpace(h.Get("monnify-signature")))
	if got == "" {
		return ErrBadSignature
	}
	want := Signature(secret, rawBody)
	if !hmac.Equal([]byte(got), []byte(want)) {
		return ErrBadSignature
	}
	return nil
}
