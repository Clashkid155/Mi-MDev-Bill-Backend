package kora

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// SignatureHeader carries the HMAC-SHA256 (hex) of the webhook's data object,
// keyed with the secret key. Only the data object is signed, not the whole body.
const SignatureHeader = "x-korapay-signature"

// VerifyWebhook checks that the webhook came from Kora. Kora signs the JSON of
// the data object; the bytes as received are tried first, then their compact
// form, in case a sender re-spaced them.
func VerifyWebhook(secret string, header http.Header, body []byte) error {
	if secret == "" {
		return errors.New("kora: no secret key to check the signature with")
	}
	got, err := hex.DecodeString(strings.TrimSpace(header.Get(SignatureHeader)))
	if err != nil || len(got) == 0 {
		return errors.New("kora: missing or unreadable signature")
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Data) == 0 {
		return errors.New("kora: webhook has no data object")
	}
	candidates := [][]byte{env.Data}
	var compact bytes.Buffer
	if json.Compact(&compact, env.Data) == nil && !bytes.Equal(compact.Bytes(), env.Data) {
		candidates = append(candidates, compact.Bytes())
	}
	for _, c := range candidates {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(c)
		if hmac.Equal(mac.Sum(nil), got) {
			return nil
		}
	}
	return errors.New("kora: signature does not match")
}
