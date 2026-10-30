package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"Bills-Backend/reply"
)

// Admin serves the admin endpoints every part of the backend shares: who am I, the server's
// version and mode, named tokens, the audit log and events. They go behind an AdminGuard.
type Admin struct {
	Store  *Store
	Log    *slog.Logger
	Alerts Alerter // tells the owner when a token is made or revoked; nil sends nothing
	Info   ServerInfo
}

// Register adds the endpoints to mux.
func (a *Admin) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/whoami", a.whoami)
	mux.HandleFunc("GET /admin/server", a.serverInfo)
	mux.HandleFunc("GET /admin/tokens", a.listTokens)
	mux.HandleFunc("POST /admin/tokens", a.createToken)
	mux.HandleFunc("DELETE /admin/tokens/{id}", a.revokeToken)
	mux.HandleFunc("GET /admin/audit", a.listAudit)
	mux.HandleFunc("GET /admin/events", a.listEvents)
	mux.HandleFunc("GET /admin/events/summary", a.eventsSummary)
}

// NotifyOwner emails the owner about something outside a purchase (a token made, an address
// locked out). It returns at once and sends nothing when no Alerter is set.
func (a *Admin) NotifyOwner(key string, throttle time.Duration, subject, body string) {
	if a.Alerts != nil {
		a.Alerts.Alert(key, throttle, subject, body)
	}
}

// The token and audit endpoints, all behind the admin guard (see AdminGuard):
//
//	GET    /admin/whoami            who the token in use is: name and role (any token)
//	GET    /admin/tokens            the named tokens, never the tokens themselves (owner only)
//	POST   /admin/tokens            {"name":"dizz phone","role":"admin"} makes one; the token is in the answer, once (owner only)
//	DELETE /admin/tokens/{id}       revokes one at once (owner only)
//	GET    /admin/audit?actor=&before_id=&limit=   what admins changed, newest first (owner and admin)

// actorWith answers 403 itself unless the request came through the guard from one of the roles.
func actorWith(w http.ResponseWriter, r *http.Request, what string, allowed ...Role) (Actor, bool) {
	a, ok := ActorFrom(r.Context())
	if ok {
		for _, role := range allowed {
			if a.Role == role {
				return a, true
			}
		}
	}
	reply.Fail(w, http.StatusForbidden, what)
	return Actor{}, false
}

func (a *Admin) whoami(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFrom(r.Context())
	if !ok {
		reply.Fail(w, http.StatusForbidden, "not signed in")
		return
	}
	reply.OK(w, http.StatusOK, map[string]any{"name": actor.Name, "role": actor.Role})
}

func (a *Admin) listTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := actorWith(w, r, "only the owner can manage tokens", RoleOwner); !ok {
		return
	}
	tokens, err := a.Store.ListAdminTokens(r.Context())
	if err != nil {
		a.Log.Error("ListTokens - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	reply.OK(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (a *Admin) createToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorWith(w, r, "only the owner can manage tokens", RoleOwner)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		Role Role   `json:"role"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil || json.Unmarshal(body, &req) != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	token, secret, err := a.Store.CreateAdminToken(r.Context(), req.Name, req.Role, actor.Name)
	var invalid InvalidError
	switch {
	case errors.As(err, &invalid):
		reply.Fail(w, http.StatusBadRequest, invalid.Message)
		return
	case errors.Is(err, ErrTokenNameTaken):
		reply.Fail(w, http.StatusConflict, "a working token with that name already exists; revoke it first or choose another name")
		return
	case err != nil:
		a.Log.Error("CreateToken - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	a.NotifyOwner(fmt.Sprintf("admin-token-made:%d", token.ID), time.Minute, "A new admin token was made",
		fmt.Sprintf("%s made the %s token %q (ends %s).\n\nIf that was not you, revoke it with DELETE /admin/tokens/%d and change ADMIN_TOKEN.",
			actor.Name, token.Role, token.Name, token.Hint, token.ID))
	reply.OK(w, http.StatusOK, map[string]any{
		"id": token.ID, "name": token.Name, "role": token.Role, "hint": token.Hint,
		"token": secret, "note": "Copy the token now: it is not stored and cannot be shown again.",
	})
}

func (a *Admin) revokeToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorWith(w, r, "only the owner can manage tokens", RoleOwner)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		reply.Fail(w, http.StatusBadRequest, "the token id must be a number from GET /admin/tokens")
		return
	}
	token, err := a.Store.RevokeAdminToken(r.Context(), id)
	switch {
	case errors.Is(err, ErrTokenNotFound):
		reply.Fail(w, http.StatusNotFound, "no working token with that id")
		return
	case err != nil:
		a.Log.Error("RevokeToken - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	a.NotifyOwner(fmt.Sprintf("admin-token-revoked:%d", token.ID), time.Minute, "An admin token was revoked",
		fmt.Sprintf("%s revoked the %s token %q (ends %s). It stopped working at once.", actor.Name, token.Role, token.Name, token.Hint))
	reply.OK(w, http.StatusOK, token)
}

func (a *Admin) listAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := actorWith(w, r, "viewer tokens cannot read the audit log", RoleOwner, RoleAdmin); !ok {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			reply.Fail(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	var before int64
	if raw := r.URL.Query().Get("before_id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			reply.Fail(w, http.StatusBadRequest, "before_id must be an entry id from an earlier answer")
			return
		}
		before = n
	}
	entries, err := a.Store.ListAudit(r.Context(), r.URL.Query().Get("actor"), before, limit)
	if err != nil {
		a.Log.Error("ListAudit - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	reply.OK(w, http.StatusOK, map[string]any{"entries": entries})
}

// GET /admin/events?level=warn&kind=order.&order=MDEV_...&since=2026-10-28&before_id=&limit=
//
// What happened, newest first. level is the lowest to show: info (everything, the default), warn
// (warnings and errors) or error. kind is an exact kind or, ending in a dot, a family: "order.".
// since is a day (2026-10-28) or a time (RFC 3339). before_id pages back; limit is 1 to 200.
// Owner and admin tokens only: events carry what the server logged, even redacted.
func (a *Admin) listEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := actorWith(w, r, "viewer tokens cannot read events", RoleOwner, RoleAdmin); !ok {
		return
	}
	q := r.URL.Query()
	f := EventFilter{MinLevel: q.Get("level"), Kind: q.Get("kind"), OrderID: q.Get("order"), Limit: 50}
	if _, known := levelRank[f.MinLevel]; f.MinLevel != "" && !known {
		reply.Fail(w, http.StatusBadRequest, "level must be info, warn or error")
		return
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			reply.Fail(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		f.Limit = n
	}
	if raw := q.Get("before_id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			reply.Fail(w, http.StatusBadRequest, "before_id must be an event id from an earlier answer")
			return
		}
		f.BeforeID = n
	}
	if raw := q.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t, err = time.ParseInLocation(dateLayout, raw, lagos)
		}
		if err != nil {
			reply.Fail(w, http.StatusBadRequest, "since must be a day like 2026-10-28 or a time like 2026-10-28T09:00:00+01:00")
			return
		}
		f.Since = t
	}
	events, err := a.Store.ListEvents(r.Context(), f)
	if err != nil {
		a.Log.Error("ListEvents - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	reply.OK(w, http.StatusOK, map[string]any{"events": events})
}

// GET /admin/events/summary?hours=24
//
// How many info, warning and error events there were lately, and the latest error: for a badge
// ("2 errors today") that opens the list. hours is 1 to 720, default 24.
func (a *Admin) eventsSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := actorWith(w, r, "viewer tokens cannot read events", RoleOwner, RoleAdmin); !ok {
		return
	}
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 720 {
			reply.Fail(w, http.StatusBadRequest, "hours must be between 1 and 720")
			return
		}
		hours = n
	}
	sum, err := a.Store.SummarizeEvents(r.Context(), time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		a.Log.Error("EventsSummary - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	reply.OK(w, http.StatusOK, sum)
}

// ServerInfo is what GET /admin/server reports about the running program. Nothing in it is secret.
type ServerInfo struct {
	Version          string    // the commit it was built from, "-uncommitted" if the tree had changes
	GoVersion        string    // the Go it was built with
	Env              string    // "production" or "sandbox" (APP_ENV)
	PaymentProvider  string    // "kora" or "monnify"
	ABSSandbox       bool      // ABS only validates and never charges or delivers
	DirectTestRoutes bool      // the /abs, /kora, /monnify and /email test routes are being served
	StartedAt        time.Time // when this process started
}

// GET /admin/server (any admin token, viewers included)
//
// Which build is running and in what mode: version (the commit), go_version, env, abs_sandbox,
// direct_test_routes, payment_provider, started_at (unix seconds) and uptime_seconds. It answers
// "is my deploy live?" and "which mode is this server in?" without reading the journal.
func (a *Admin) serverInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := ActorFrom(r.Context()); !ok {
		reply.Fail(w, http.StatusForbidden, "not signed in")
		return
	}
	i := a.Info
	uptime := int64(0)
	if !i.StartedAt.IsZero() {
		uptime = int64(time.Since(i.StartedAt).Seconds())
	}
	reply.OK(w, http.StatusOK, map[string]any{
		"version": i.Version, "go_version": i.GoVersion, "env": i.Env, "payment_provider": i.PaymentProvider,
		"abs_sandbox": i.ABSSandbox, "direct_test_routes": i.DirectTestRoutes,
		"started_at": i.StartedAt.Unix(), "uptime_seconds": uptime,
	})
}
