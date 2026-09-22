package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireAdmin(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	call := func(h http.Handler, auth string) int {
		req := httptest.NewRequest("GET", "/abs/catalog", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	guarded := requireAdmin("s3cret", ok)
	for name, auth := range map[string]string{"no header": "", "wrong token": "Bearer wrong", "no Bearer prefix": "s3cret"} {
		if code := call(guarded, auth); code != 401 {
			t.Errorf("%s: got %d, want 401", name, code)
		}
	}
	if call(guarded, "Bearer s3cret") != 200 {
		t.Error("right token rejected")
	}
	if call(requireAdmin("", ok), "") != 200 {
		t.Error("empty token should leave the routes open (sandbox only)")
	}
}
