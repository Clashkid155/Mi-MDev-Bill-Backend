package logging

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Requests logs one line per HTTP request served:
//
//	[INFO]   [HTTP]  POST /v1/orders -> 200 · 84ms
//
// 4xx responses log as WARN and 5xx as ERROR.
func Requests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		detail := fmt.Sprintf("%s -> %d · %s", routeOf(r), rec.status, Duration(time.Since(started)))
		switch {
		case rec.status >= 500:
			log.Error("HTTP", "detail", detail)
		case rec.status >= 400:
			log.Warn("HTTP", "detail", detail)
		default:
			log.Info("HTTP", "detail", detail)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// routeOf names what was asked for. When the router matched a pattern it is
// logged instead of the path, so "GET /v1/orders/{id}" is written rather than a
// customer's order id (which is all that protects an order's status page).
func routeOf(r *http.Request) string {
	pattern := r.Pattern
	switch {
	case pattern == "" || pattern == "/":
		return r.Method + " " + r.URL.Path
	case strings.Contains(pattern, " "):
		return pattern
	default:
		return r.Method + " " + pattern
	}
}
