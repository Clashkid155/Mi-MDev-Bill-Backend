package monnify

import (
	"net/http"
	"testing"
)

// Round-trip check; not an official Monnify vector.
func TestVerifyWebhook(t *testing.T) {
	secret := "91MUDL9N6U3BQRXBQ2PJ9M0PW4J22M1Y"
	body := []byte("{\"eventType\":\"x\"}")
	sig := Signature(secret, body)

	h := http.Header{}
	h.Set("monnify-signature", sig)
	if err := VerifyWebhook(secret, h, body); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifyWebhook(secret, h, append(body, ' ')); err == nil {
		t.Fatal("tampered body accepted")
	}
	if err := VerifyWebhook(secret, http.Header{}, body); err == nil {
		t.Fatal("missing signature accepted")
	}
}
