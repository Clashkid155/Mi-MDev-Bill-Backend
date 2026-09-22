package main

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"Bills-Backend/reply"
)

// requireAdmin protects the direct /abs and /monnify endpoints, which can spend
// the ABS wallet. With an empty token it lets everything through; config.go
// refuses that combination whenever real money can be spent.
func requireAdmin(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, hasPrefix := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !hasPrefix || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			reply.Fail(w, http.StatusUnauthorized, "admin token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
