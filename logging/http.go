package logging

import (
	"fmt"
	"log/slog"
	"net/http"
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

		detail := fmt.Sprintf("%s %s -> %d · %s", r.Method, r.URL.Path, rec.status, Duration(time.Since(started)))
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
