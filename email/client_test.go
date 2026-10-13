package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func settings(url string) Settings {
	return Settings{BaseURL: url, Token: "tok123", FromEmail: "orders@mi.app", FromName: "Mi MDev"}
}

func TestSendShapesTheRequestForZeptoMail(t *testing.T) {
	var gotAuth, gotPath, gotType string
	var gotBody sendBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotType = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"data":[{"code":"EM_104","message":"OK"}],"message":"OK","request_id":"req-1"}`))
	}))
	defer srv.Close()

	s := settings(srv.URL)
	s.BounceAddress = "bounce@mi.app"
	err := New(s).Send(context.Background(), Message{To: "a@b.co", ToName: "A", Subject: "Hi", Text: "plain", HTML: "<p>rich</p>"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Zoho-enczapikey tok123" || gotPath != "/v1.1/email" || gotType != "application/json" {
		t.Fatalf("auth %q path %q type %q", gotAuth, gotPath, gotType)
	}
	if gotBody.From.Address != "orders@mi.app" || gotBody.From.Name != "Mi MDev" ||
		len(gotBody.To) != 1 || gotBody.To[0].EmailAddress.Address != "a@b.co" || gotBody.To[0].EmailAddress.Name != "A" ||
		gotBody.Subject != "Hi" || gotBody.TextBody != "plain" || gotBody.HTMLBody != "<p>rich</p>" ||
		gotBody.BounceAddress != "bounce@mi.app" {
		t.Fatalf("bad body: %+v", gotBody)
	}
}

// ZeptoMail's console shows the whole header value, so a token pasted with the
// prefix must not end up with it twice.
func TestTokenPastedWithItsPrefixIsAccepted(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[],"message":"OK"}`))
	}))
	defer srv.Close()

	s := settings(srv.URL + "/") // trailing slash on the base URL is tolerated too
	s.Token = "  Zoho-enczapikey tok123  "
	if err := New(s).Send(context.Background(), Message{To: "a@b.co", Subject: "Hi", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Zoho-enczapikey tok123" {
		t.Fatalf("auth header %q", gotAuth)
	}
}

func TestBounceAddressIsOmittedWhenNotSet(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&raw)
		w.Write([]byte(`{"data":[],"message":"OK"}`))
	}))
	defer srv.Close()
	if err := New(settings(srv.URL)).Send(context.Background(), Message{To: "a@b.co", Subject: "Hi", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, has := raw["bounce_address"]; has {
		t.Fatal("bounce_address must not be sent when empty")
	}
}

// ZeptoMail has no sandbox of its own: ours never contacts it.
func TestSandboxNeverCallsZeptoMail(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	s := settings(srv.URL)
	s.Sandbox = true
	c := New(s)
	if err := c.Send(context.Background(), Message{To: "a@b.co", Subject: "Hi", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || !c.Sandbox() {
		t.Fatalf("calls %d, sandbox %v", calls.Load(), c.Sandbox())
	}
}

func TestSendRequiresABody(t *testing.T) {
	if err := New(settings("http://unused")).Send(context.Background(), Message{To: "x@y.co", Subject: "Hi"}); err == nil {
		t.Fatal("message with no body accepted")
	}
}

func TestSendReportsZeptoMailsErrorCodeAndMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"data":{"error_code":"TM_3004","message":"Invalid request"},"message":"error"}`))
	}))
	defer srv.Close()
	err := New(settings(srv.URL)).Send(context.Background(), Message{To: "x@y.co", Subject: "Hi", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "TM_3004") || !strings.Contains(err.Error(), "Invalid request") {
		t.Fatalf("got %v", err)
	}
}

func TestSendFailsOnAnUnreadableErrorReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte(`<html>bad gateway</html>`))
	}))
	defer srv.Close()
	err := New(settings(srv.URL)).Send(context.Background(), Message{To: "x@y.co", Subject: "Hi", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("got %v", err)
	}
}

func TestDirectSendRoute(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"data":[],"message":"OK"}`))
	}))
	defer srv.Close()

	post := func(c *Client, body string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		c.RegisterDirectRoutes(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/email/send", strings.NewReader(body)))
		return rec
	}

	rec := post(New(settings(srv.URL)), `{"to":"x@y.co","subject":"Hi","text":"body"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"successful"`) || calls.Load() != 1 {
		t.Fatalf("%d %s calls=%d", rec.Code, rec.Body, calls.Load())
	}
	if rec := post(New(settings(srv.URL)), `{"to":"x@y.co"}`); rec.Code != 400 {
		t.Fatalf("missing subject/body accepted: %d", rec.Code)
	}

	sandbox := settings(srv.URL)
	sandbox.Sandbox = true
	rec = post(New(sandbox), `{"to":"x@y.co","subject":"Hi","text":"body"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "not sent") || calls.Load() != 1 {
		t.Fatalf("sandbox must say so and not send: %d %s calls=%d", rec.Code, rec.Body, calls.Load())
	}
}
