package abs

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func balanceClient(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/profile/get_user_detail.php" || r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("unexpected request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "key", true)
}

func TestBalance(t *testing.T) {
	c := balanceClient(t, 200, `{"status":true,"data":{"apikey":"****","user_wallets":[{"name":"USD","walletbal":"9.00"},{"name":"NGN","walletbal":"726.00"}]}}`)
	if kobo, err := c.Balance(context.Background()); err != nil || kobo != 72600 {
		t.Fatalf("got %d, %v", kobo, err)
	}
	for name, c := range map[string]*Client{
		"no naira wallet": balanceClient(t, 200, `{"status":true,"data":{"user_wallets":[{"name":"USD","walletbal":"9"}]}}`),
		"unreadable":      balanceClient(t, 200, `{"status":true,"data":{"user_wallets":[{"name":"NGN","walletbal":"lots"}]}}`),
		"ABS says failed": balanceClient(t, 200, `{"status":false,"text":"nope"}`),
		"server error":    balanceClient(t, 500, `oops`),
		"not json":        balanceClient(t, 200, `<html>`),
	} {
		if _, err := c.Balance(context.Background()); err == nil {
			t.Errorf("%s: should be an error, never a made up balance", name)
		}
	}
}

func TestBalanceNeverQuotesTheProfileReplyInTheLog(t *testing.T) {
	var logs strings.Builder
	c := balanceClient(t, 403, `{"status":false,"text":"Forbidden","data":{"email":"owner@example.com","phone":"0801"}}`)
	c.Log = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := c.Balance(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(logs.String(), "owner@example.com") || strings.Contains(logs.String(), "0801") || !strings.Contains(logs.String(), "GetBalance") {
		t.Errorf("log: %s", logs.String())
	}
}
