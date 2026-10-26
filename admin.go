package main

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"Bills-Backend/ratelimit"
	"Bills-Backend/reply"
)

// requireAdmin protects the admin routes and the direct /abs and /monnify
// endpoints, which can spend the ABS wallet. With an empty token it lets
// everything through; config.go refuses that combination whenever real money can
// be spent.
//
// fails (optional) locks out a client address after too many wrong tokens in a
// row. A locked address is refused with 429 before its token is even looked at,
// so the token cannot be guessed at speed. A right token clears the count.
func requireAdmin(token string, fails *ratelimit.Lockout, trustProxy bool, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := ratelimit.ClientIP(r, trustProxy)
		if fails != nil {
			if wait, blocked := fails.Blocked(who); blocked {
				ratelimit.Locked(w, wait)
				return
			}
		}
		got, hasPrefix := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !hasPrefix || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			if fails != nil {
				fails.Fail(who)
			}
			reply.Fail(w, http.StatusUnauthorized, "admin token required")
			return
		}
		if fails != nil {
			fails.Succeed(who)
		}
		next.ServeHTTP(w, r)
	})
}
