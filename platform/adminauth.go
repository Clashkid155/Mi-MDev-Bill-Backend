package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"Bills-Backend/ratelimit"
	"Bills-Backend/reply"
)

// Role says what an admin token may do.
type Role string

const (
	// RoleOwner is the ADMIN_TOKEN from the settings file: everything, including making and
	// revoking the other tokens.
	RoleOwner Role = "owner"
	// RoleAdmin may read and change everything except tokens.
	RoleAdmin Role = "admin"
	// RoleViewer may only read: a dashboard that shows figures but cannot refund or change a setting.
	RoleViewer Role = "viewer"
)

// Actor is who is making an admin request.
type Actor struct {
	ID   int64 // admin_tokens.id; 0 for the owner
	Name string
	Role Role
}

type actorKey struct{}

// WithActor puts the actor on the request's context; the admin guard does this.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom is who is making the request, if it came through the admin guard.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// tokenPrefix starts every named token, so that one found in a log or a chat is recognisable.
const tokenPrefix = "mdev_"

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AdminToken is a named token as listed: never the token itself.
type AdminToken struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Role       Role   `json:"role"`
	Hint       string `json:"hint"` // the last 4 characters
	CreatedAt  int64  `json:"created_at"`
	CreatedBy  string `json:"created_by"`
	LastUsedAt int64  `json:"last_used_at"` // 0 if never used
	LastUsedIP string `json:"last_used_ip,omitempty"`
	RevokedAt  int64  `json:"revoked_at"` // 0 while it works
}

var (
	// ErrTokenNameTaken means a working token already has that name.
	ErrTokenNameTaken = errors.New("order: a token with that name already exists")
	// ErrTokenNotFound means no such token, or it was already revoked.
	ErrTokenNotFound = errors.New("order: no such working token")

	tokenNameRule = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,39}$`)
)

// reservedNames cannot be given to a token: they name the owner and the open sandbox mode in the audit log.
var reservedNames = map[string]bool{"owner": true, "open": true, "(unknown)": true}

// validateTokenRequest says why a token name or role is refused; nil when it is fine.
func validateTokenRequest(name string, role Role) error {
	if !tokenNameRule.MatchString(name) {
		return InvalidError{"name must be 1 to 40 letters, digits, spaces, dots, dashes or underscores, starting with a letter or digit"}
	}
	if reservedNames[strings.ToLower(name)] {
		return InvalidError{`"` + name + `" is reserved; choose another name`}
	}
	if role != RoleAdmin && role != RoleViewer {
		return InvalidError{`role must be "admin" or "viewer"`}
	}
	return nil
}

// CreateAdminToken makes a named token and returns it. The token is only ever returned here; the
// database keeps a hash, so it cannot be shown again.
func (s *Store) CreateAdminToken(ctx context.Context, name string, role Role, createdBy string) (AdminToken, string, error) {
	if err := validateTokenRequest(name, role); err != nil {
		return AdminToken{}, "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return AdminToken{}, "", err
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `INSERT INTO admin_tokens (name, role, token_hash, hint, created_at, created_by) VALUES (?,?,?,?,?,?)`,
		name, string(role), hashToken(token), token[len(token)-4:], now, createdBy)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return AdminToken{}, "", ErrTokenNameTaken
		}
		return AdminToken{}, "", err
	}
	id, _ := res.LastInsertId()
	return AdminToken{ID: id, Name: name, Role: role, Hint: token[len(token)-4:], CreatedAt: now, CreatedBy: createdBy}, token, nil
}

// ListAdminTokens lists every token, working ones first.
func (s *Store) ListAdminTokens(ctx context.Context) ([]AdminToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, role, hint, created_at, created_by, last_used_at, last_used_ip, revoked_at
		FROM admin_tokens ORDER BY (revoked_at > 0), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminToken{}
	for rows.Next() {
		var t AdminToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.Hint, &t.CreatedAt, &t.CreatedBy, &t.LastUsedAt, &t.LastUsedIP, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAdminToken stops a token working, at once and for good. The row stays, so the audit log
// can still name it.
func (s *Store) RevokeAdminToken(ctx context.Context, id int64) (AdminToken, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE admin_tokens SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, time.Now().Unix(), id)
	if err != nil {
		return AdminToken{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return AdminToken{}, ErrTokenNotFound
	}
	var t AdminToken
	err = s.db.QueryRowContext(ctx, `SELECT id, name, role, hint, created_at, created_by, last_used_at, last_used_ip, revoked_at FROM admin_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.Role, &t.Hint, &t.CreatedAt, &t.CreatedBy, &t.LastUsedAt, &t.LastUsedIP, &t.RevokedAt)
	return t, err
}

// lookupAdminToken finds the working token that was presented. Tokens are 256 random bits, so
// looking the hash up directly leaks nothing useful.
func (s *Store) lookupAdminToken(ctx context.Context, presented string) (Actor, bool, error) {
	var a Actor
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, role FROM admin_tokens WHERE token_hash = ? AND revoked_at = 0`, hashToken(presented)).Scan(&a.ID, &a.Name, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return Actor{}, false, nil
	}
	if err != nil {
		return Actor{}, false, err
	}
	a.Role = Role(role)
	return a, true, nil
}

func (s *Store) touchAdminToken(ctx context.Context, id int64, ip string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE admin_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`, time.Now().Unix(), ip, id)
}

// ---- the audit log ----

// AuditEntry is one thing an admin did.
type AuditEntry struct {
	ID      int64  `json:"id"`
	At      int64  `json:"at"`
	Actor   string `json:"actor"`
	ActorID int64  `json:"actor_id"`
	Role    string `json:"role"`
	IP      string `json:"ip"`
	Method  string `json:"method"`
	Route   string `json:"route"`
	Path    string `json:"path"`
	Status  int    `json:"status"`
	Detail  string `json:"detail"`
}

// AddAudit records one entry (At is filled in when zero).
func (s *Store) AddAudit(ctx context.Context, e AuditEntry) error {
	if e.At == 0 {
		e.At = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (at, actor, actor_id, role, ip, method, route, path, status, detail) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.At, e.Actor, e.ActorID, e.Role, e.IP, e.Method, e.Route, e.Path, e.Status, e.Detail)
	return err
}

// ListAudit returns entries newest first: only those by actor when given, only those older than
// beforeID when given (for paging), at most limit.
func (s *Store) ListAudit(ctx context.Context, actor string, beforeID int64, limit int) ([]AuditEntry, error) {
	query := `SELECT id, at, actor, actor_id, role, ip, method, route, path, status, detail FROM audit_log WHERE 1=1`
	args := []any{}
	if actor != "" {
		query += ` AND actor = ?`
		args = append(args, actor)
	}
	if beforeID > 0 {
		query += ` AND id < ?`
		args = append(args, beforeID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.ActorID, &e.Role, &e.IP, &e.Method, &e.Route, &e.Path, &e.Status, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- the guard ----

// AdminGuard protects the admin routes (and the direct provider routes in sandbox). It accepts the
// owner token or a named token, refuses a viewer anything but reading, locks an address out after
// too many wrong tokens, and writes every change to the audit log.
type AdminGuard struct {
	Store *Store
	// Owner is the ADMIN_TOKEN. Empty leaves the routes open, as the owner called "open": sandbox
	// only, because config.go refuses it whenever real money can be spent.
	Owner      string
	Fails      *ratelimit.Lockout
	TrustProxy bool
	Log        *slog.Logger

	lastTouch sync.Map // token id -> when its last use was stored
}

// Wrap puts the guard in front of next.
func (g *AdminGuard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ratelimit.ClientIP(r, g.TrustProxy)

		var actor Actor
		if g.Owner == "" {
			actor = Actor{Name: "open", Role: RoleOwner}
		} else {
			if g.Fails != nil {
				if wait, blocked := g.Fails.Blocked(ip); blocked {
					ratelimit.Locked(w, wait)
					return
				}
			}
			a, ok := g.authenticate(r, ip)
			if !ok {
				if g.Fails != nil {
					g.Fails.Fail(ip)
				}
				reply.Fail(w, http.StatusUnauthorized, "admin token required")
				return
			}
			if g.Fails != nil {
				g.Fails.Succeed(ip)
			}
			actor = a
		}

		writes := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if writes && actor.Role == RoleViewer {
			g.record(r, actor, ip, http.StatusForbidden, "refused: a viewer token cannot change anything")
			reply.Fail(w, http.StatusForbidden, "this token is read only")
			return
		}

		inner := r.WithContext(WithActor(r.Context(), actor))
		var tap *bodyTap
		if writes {
			tap = &bodyTap{ReadCloser: inner.Body}
			inner.Body = tap
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, inner)
		r.Pattern = inner.Pattern // the access log names the route from the request it holds

		if writes {
			g.record(inner, actor, ip, rec.status, tap.detail())
		}
	})
}

func (g *AdminGuard) authenticate(r *http.Request, ip string) (Actor, bool) {
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || presented == "" {
		return Actor{}, false
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(g.Owner)) == 1 {
		return Actor{Name: "owner", Role: RoleOwner}, true
	}
	if g.Store == nil || !strings.HasPrefix(presented, tokenPrefix) {
		return Actor{}, false
	}
	a, found, err := g.Store.lookupAdminToken(r.Context(), presented)
	if err != nil {
		if g.Log != nil {
			g.Log.Error("AdminGuard - API", "detail", "could not look up a token: "+err.Error())
		}
		return Actor{}, false
	}
	if found {
		// Remember when it was last used, but write that at most once a minute.
		if last, ok := g.lastTouch.Load(a.ID); !ok || time.Since(last.(time.Time)) > time.Minute {
			g.lastTouch.Store(a.ID, time.Now())
			g.Store.touchAdminToken(r.Context(), a.ID, ip)
		}
	}
	return a, found
}

// record writes one audit entry; a failure to write is logged and never stops the request.
func (g *AdminGuard) record(r *http.Request, a Actor, ip string, status int, detail string) {
	if g.Store == nil {
		return
	}
	// A refused request never reached the router, so it has no route of its own (the
	// pattern on it is just the "/admin/" prefix): name it by method and path instead.
	route := r.Pattern
	if !strings.Contains(route, " ") {
		route = r.Method + " " + r.URL.Path
	}
	err := g.Store.AddAudit(context.WithoutCancel(r.Context()), AuditEntry{
		Actor: a.Name, ActorID: a.ID, Role: string(a.Role), IP: ip,
		Method: r.Method, Route: route, Path: truncate(r.URL.Path, 200), Status: status, Detail: detail,
	})
	if err != nil && g.Log != nil {
		g.Log.Error("AdminGuard - API", "detail", "could not write the audit log: "+err.Error())
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// statusRecorder remembers the status a handler answered with.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// bodyTap keeps the first bytes of a request body as the handler reads it, for the audit log.
type bodyTap struct {
	io.ReadCloser
	buf  bytes.Buffer
	seen int
}

const auditBodyMax = 2048

func (t *bodyTap) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.seen += n
	if room := auditBodyMax - t.buf.Len(); room > 0 && n > 0 {
		t.buf.Write(p[:min(n, room)])
	}
	return n, err
}

var secretField = regexp.MustCompile(`(?i)token|secret|password|authorization|api_?key`)

// detail is what the handler was sent, with any field that looks secret blanked.
func (t *bodyTap) detail() string {
	if t.seen == 0 {
		return ""
	}
	if t.seen > auditBodyMax {
		return fmt.Sprintf("(%d bytes, too large to keep)", t.seen)
	}
	var fields map[string]any
	if err := json.Unmarshal(t.buf.Bytes(), &fields); err != nil {
		return fmt.Sprintf("(%d bytes, not JSON)", t.seen)
	}
	for k := range fields {
		if secretField.MatchString(k) {
			fields[k] = "[hidden]"
		}
	}
	out, _ := json.Marshal(fields)
	return string(out)
}
