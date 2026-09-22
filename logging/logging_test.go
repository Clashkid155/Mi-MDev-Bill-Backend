package logging

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFormat(t *testing.T) {
	var out bytes.Buffer
	log := New(&out, slog.LevelInfo)

	log.Info("GetSystemData - ABS", "detail", "HTTP 200 · 640.0 KB")
	log.Warn("BuyAirtime - ABS", "detail", "FAILED\n  code 141:   invalid id", "order", "abc")
	log.Error("CreateOrder - API", "detail", "boom")
	log.Debug("hidden", "detail", "not shown")

	want := "[INFO]   [GetSystemData - ABS]  HTTP 200 · 640.0 KB\n" +
		"[WARN]   [BuyAirtime - ABS]  FAILED code 141: invalid id  order=abc\n" +
		"[ERROR]  [CreateOrder - API]  boom\n"
	if out.String() != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out.String(), want)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("colour codes written to a non-terminal")
	}
}

func TestRequests(t *testing.T) {
	var out bytes.Buffer
	handler := Requests(New(&out, slog.LevelInfo), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/orders", nil))

	if got := out.String(); !strings.HasPrefix(got, "[WARN]   [HTTP]  POST /v1/orders -> 400 · ") {
		t.Fatalf("got %q", got)
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[int]string{512: "512 B", 3500: "3.4 KB", 2 << 20: "2.0 MB"} {
		if got := Bytes(n); got != want {
			t.Errorf("%d: got %s want %s", n, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{200 * time.Microsecond: "<1ms", 84 * time.Millisecond: "84ms", 1240 * time.Millisecond: "1.2s"} {
		if got := Duration(d); got != want {
			t.Errorf("%v: got %s want %s", d, got, want)
		}
	}
}
