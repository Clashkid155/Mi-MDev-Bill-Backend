package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"Bills-Backend/logging"
	"Bills-Backend/notify"
	"Bills-Backend/order"
	"Bills-Backend/platform"
	"Bills-Backend/ratelimit"
	"Bills-Backend/reply"
)

// How much one client address may ask of the public endpoints. Mobile carriers
// put many customers behind one address and the app polls an open order about
// 12 times a minute, so reading is generous: it is a cheap lookup, and this only
// stops floods. Writing is tighter because each call can reach a provider.
const (
	readsPerMinute, readsBurst   = 600, 200 // catalog, order status
	writesPerMinute, writesBurst = 20, 10   // new orders
	metersPerMinute, metersBurst = 6, 5     // meter checks, per address

	// ABS allows our key about 300 calls an hour, and every meter check is one.
	// Everyone together may make about 120 an hour (2 a minute after a burst of
	// 30), so a single caller cannot use up the quota that purchases need.
	metersTotalPerMinute, metersTotalBurst = 2, 30
)

// An address that gets the admin token wrong this many times in a row is blocked
// for adminLockFor, even if its next try is right.
const (
	adminMaxWrongTokens = 5
	adminLockFor        = 15 * time.Minute
)

// routes puts every endpoint together: the public API behind its rate limits,
// the admin routes behind the token, and security headers on everything. direct
// is the providers' direct endpoints, or nil when real money is in use.
func routes(cfg Config, log *slog.Logger, api *order.API, admin *platform.Admin, notices *notify.Service, direct http.Handler) http.Handler {
	fails := ratelimit.NewLockout(adminMaxWrongTokens, adminLockFor)
	guard := &platform.AdminGuard{Store: admin.Store, Owner: cfg.AdminToken, Fails: fails, TrustProxy: cfg.TrustProxy, Log: log}
	fails.OnLock = func(who string, blockFor time.Duration) {
		admin.NotifyOwner("admin-lockout:"+who, time.Hour, "Someone is guessing the admin token",
			fmt.Sprintf("The address %s got the admin token wrong %d times in a row and is blocked for %d minutes.\n\n"+
				"If that was you, nothing more is needed. If not, nobody has got in: the right token is still refused while blocked. "+
				"If it keeps happening, change ADMIN_TOKEN.", who, adminMaxWrongTokens, int(blockFor.Minutes())))
		if err := admin.Store.AddAudit(context.Background(), platform.AuditEntry{
			Actor: "(unknown)", IP: who, Route: "admin lockout", Status: http.StatusTooManyRequests,
			Detail: fmt.Sprintf("%d wrong tokens in a row; blocked for %d minutes", adminMaxWrongTokens, int(blockFor.Minutes())),
		}); err != nil {
			log.Error("Admin - API", "detail", "could not write the audit log: "+err.Error())
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		reply.OKWithMessage(w, http.StatusOK, "ok", nil)
	})
	// The apps' endpoints: bills (orders, catalog) and notices share one table and one set of limits.
	publicMux := http.NewServeMux()
	publicMux.Handle("/", api.Routes())
	notices.RegisterPublic(publicMux)
	mux.Handle("/", limitPublic(cfg.TrustProxy, publicMux))
	// Direct access to the providers, for curl and Postman. These can spend real
	// money, so they are only served while nothing real is at stake.
	if direct != nil {
		for _, prefix := range []string{"/abs/", "/monnify/", "/kora/", "/email/"} {
			mux.Handle(prefix, guard.Wrap(direct))
		}
	}
	// Every feature adds its admin endpoints to the same table; one guard protects them all.
	adminMux := http.NewServeMux()
	api.RegisterAdmin(adminMux)
	admin.Register(adminMux)
	notices.RegisterAdmin(adminMux)
	mux.Handle("/admin/", guard.Wrap(adminMux))
	return secureHeaders(logging.Requests(log, mux))
}

// limitPublic applies the per-address limits to the app's endpoints. Webhooks
// are not limited (the providers call from a few addresses and every one is
// signature checked); the health check is not either.
func limitPublic(trustProxy bool, next http.Handler) http.Handler {
	reads := ratelimit.New(readsPerMinute, readsBurst).Middleware(trustProxy, next)
	writes := ratelimit.New(writesPerMinute, writesBurst).Middleware(trustProxy, next)
	meters := ratelimit.New(metersPerMinute, metersBurst).Middleware(trustProxy,
		ratelimit.New(metersTotalPerMinute, metersTotalBurst).Shared(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !strings.HasPrefix(r.URL.Path, "/v1/"):
			next.ServeHTTP(w, r)
		case r.Method == http.MethodGet:
			reads.ServeHTTP(w, r)
		case r.URL.Path == "/v1/meters/validate":
			meters.ServeHTTP(w, r)
		default:
			writes.ServeHTTP(w, r)
		}
	})
}

// secureHeaders stops browsers and proxies from keeping or guessing at answers
// that can hold a customer's electricity token or an admin's figures.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
