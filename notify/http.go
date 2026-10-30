package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"Bills-Backend/platform"
	"Bills-Backend/reply"
)

// Service serves the notices and answers the maintenance gate.
type Service struct {
	Store  *Store
	Log    *slog.Logger
	Events *platform.EventRecorder // notes that a notice was made, changed or ended; nil records nothing
	Now    func() time.Time        // for tests; nil means time.Now
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const maxBody = 1 << 16

// RegisterPublic adds the endpoint the apps call. It needs no login: the apps have no accounts.
//
//	GET /v1/notifications?app=mi-mdev&platform=android&version=1.4.2
//
// app, platform and version may instead be sent as the headers X-App, X-Platform and X-App-Version
// (the apps should send them on every request). Answers the notices showing now that apply to the
// caller, most urgent first, plus server_time (unix seconds) so an app can tell when its clock is off.
func (s *Service) RegisterPublic(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/notifications", s.listPublic)
}

// RegisterAdmin adds the admin endpoints (behind the admin guard):
//
//	GET    /admin/notifications?status=active|upcoming|ended&limit=   every notice, newest first (any admin token)
//	POST   /admin/notifications                                       make one (owner and admin tokens)
//	PUT    /admin/notifications/{id}                                  change one that has not ended
//	DELETE /admin/notifications/{id}                                  end one now (it stays in the list as ended)
func (s *Service) RegisterAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/notifications", s.listAdmin)
	mux.HandleFunc("POST /admin/notifications", s.create)
	mux.HandleFunc("PUT /admin/notifications/{id}", s.update)
	mux.HandleFunc("DELETE /admin/notifications/{id}", s.end)
}

// ---- what an app sees ----

type publicView struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	LinkURL     string `json:"link_url,omitempty"`
	LinkLabel   string `json:"link_label,omitempty"`
	StartsAt    int64  `json:"starts_at"`
	EndsAt      int64  `json:"ends_at"` // 0 means until it is ended
	Dismissible bool   `json:"dismissible"`
	Blocking    bool   `json:"blocking"`
	BlockingNow bool   `json:"blocking_now"` // new orders are being refused right now
	BlockFrom   int64  `json:"block_from"`
	BlockUntil  int64  `json:"block_until"`
	UpdatedAt   int64  `json:"updated_at"` // an edited notice has a new value: show it again to those who dismissed it
}

func viewOf(n Notice, t int64) publicView {
	from, until := n.blockWindow()
	if !n.Blocking {
		from, until = 0, 0
	}
	now := n.blockingAt(t)
	return publicView{
		ID: n.ID, Kind: n.Kind, Title: n.Title, Body: n.Body, LinkURL: n.LinkURL, LinkLabel: n.LinkLabel,
		StartsAt: n.StartsAt, EndsAt: n.end(), Dismissible: n.Dismissible && !now, Blocking: n.Blocking, BlockingNow: now,
		BlockFrom: from, BlockUntil: until, UpdatedAt: n.UpdatedAt,
	}
}

var kindRank = map[string]int{KindMaintenance: 0, KindWarning: 1, KindInfo: 2}

func (s *Service) listPublic(w http.ResponseWriter, r *http.Request) {
	caller := platform.CallerFrom(r)
	now := s.now()
	showing, err := s.Store.Showing(r.Context(), now)
	if err != nil {
		s.Log.Error("Notifications - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	t := now.Unix()
	var mine []Notice
	for _, n := range showing {
		if n.matches(caller) {
			mine = append(mine, n)
		}
	}
	// Orders being refused first, then by kind, then newest.
	sort.SliceStable(mine, func(i, j int) bool {
		bi, bj := mine[i].blockingAt(t), mine[j].blockingAt(t)
		if bi != bj {
			return bi
		}
		if ki, kj := kindRank[mine[i].Kind], kindRank[mine[j].Kind]; ki != kj {
			return ki < kj
		}
		return mine[i].ID > mine[j].ID
	})
	const most = 20
	if len(mine) > most {
		mine = mine[:most]
	}
	views := make([]publicView, 0, len(mine))
	for _, n := range mine {
		views = append(views, viewOf(n, t))
	}
	reply.OK(w, http.StatusOK, map[string]any{"notifications": views, "server_time": t})
}

// ---- the maintenance gate ----

// OrdersBlocked says whether new orders are being refused for this caller: a notice that blocks, whose
// block window is open, and that is for the caller. When several apply, the one that lasts longest.
func (s *Service) OrdersBlocked(ctx context.Context, c platform.Caller) (platform.Block, bool) {
	now := s.now()
	showing, err := s.Store.Showing(ctx, now)
	if err != nil {
		// Never stop orders because the notices could not be read.
		s.Log.Error("Notifications - Gate", "detail", err.Error())
		return platform.Block{}, false
	}
	t := now.Unix()
	var best *Notice
	var bestUntil int64
	for i := range showing {
		n := showing[i]
		if !n.blockingAt(t) || !n.matches(c) {
			continue
		}
		_, until := n.blockWindow()
		if best == nil || until == 0 || (bestUntil != 0 && until > bestUntil) {
			best, bestUntil = &showing[i], until
		}
	}
	if best == nil {
		return platform.Block{}, false
	}
	message := best.Body
	if message == "" {
		message = best.Title
	}
	return platform.Block{NoticeID: best.ID, Title: best.Title, Message: message, Until: bestUntil}, true
}

// ---- admin ----

type adminView struct {
	publicView
	Lang        string   `json:"lang"`
	Status      string   `json:"status"` // upcoming, active or ended
	EndedAt     int64    `json:"ended_at"`
	Apps        []string `json:"apps"`
	Platforms   []string `json:"platforms"`
	MinVersion  string   `json:"min_version"`
	MaxVersion  string   `json:"max_version"`
	CreatedBy   string   `json:"created_by"`
	CreatedAt   int64    `json:"created_at"`
	Dismissible bool     `json:"dismissible"` // as set, not as shown while it blocks
}

func adminViewOf(n Notice, t int64) adminView {
	return adminView{
		publicView: viewOf(n, t), Lang: n.Lang, Status: n.status(t), EndedAt: n.EndedAt, Apps: n.Apps, Platforms: n.Platforms,
		MinVersion: n.MinVersion, MaxVersion: n.MaxVersion, CreatedBy: n.CreatedBy, CreatedAt: n.CreatedAt, Dismissible: n.Dismissible,
	}
}

func (s *Service) listAdmin(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			reply.Fail(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	want := r.URL.Query().Get("status")
	if want != "" && want != "active" && want != "upcoming" && want != "ended" {
		reply.Fail(w, http.StatusBadRequest, "status must be active, upcoming or ended")
		return
	}
	all, err := s.Store.All(r.Context(), 500)
	if err != nil {
		s.Log.Error("Notifications - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	t := s.now().Unix()
	views := []adminView{}
	for _, n := range all {
		if want != "" && n.status(t) != want {
			continue
		}
		if len(views) < limit {
			views = append(views, adminViewOf(n, t))
		}
	}
	reply.OK(w, http.StatusOK, map[string]any{"notifications": views})
}

// read decodes and checks a request body; it answers 400 itself and returns false when it is wrong.
func (s *Service) read(w http.ResponseWriter, r *http.Request) (Notice, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var in Input
	if err != nil || json.Unmarshal(body, &in) != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return Notice{}, false
	}
	n, err := in.build(s.now())
	var invalid platform.InvalidError
	if errors.As(err, &invalid) {
		reply.Fail(w, http.StatusBadRequest, invalid.Message)
		return Notice{}, false
	}
	return n, true
}

func actorName(r *http.Request) string {
	if a, ok := platform.ActorFrom(r.Context()); ok {
		return a.Name
	}
	return "unknown"
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	n, ok := s.read(w, r)
	if !ok {
		return
	}
	n.CreatedBy = actorName(r)
	made, err := s.Store.Create(r.Context(), n, s.now())
	if err != nil {
		s.Log.Error("Notifications - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.note("notice.created", made, "published")
	reply.OK(w, http.StatusOK, adminViewOf(made, s.now().Unix()))
}

func idOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		reply.Fail(w, http.StatusBadRequest, "the notice id must be a number from GET /admin/notifications")
		return 0, false
	}
	return id, true
}

func (s *Service) update(w http.ResponseWriter, r *http.Request) {
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	n, ok := s.read(w, r)
	if !ok {
		return
	}
	changed, err := s.Store.Update(r.Context(), id, n, s.now())
	if s.refused(w, err) {
		return
	}
	s.note("notice.updated", changed, "changed")
	reply.OK(w, http.StatusOK, adminViewOf(changed, s.now().Unix()))
}

func (s *Service) end(w http.ResponseWriter, r *http.Request) {
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	ended, err := s.Store.End(r.Context(), id, s.now())
	if s.refused(w, err) {
		return
	}
	s.note("notice.ended", ended, "ended")
	reply.OK(w, http.StatusOK, adminViewOf(ended, s.now().Unix()))
}

// refused answers for the errors a store call can return; true means it answered.
func (s *Service) refused(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		reply.Fail(w, http.StatusNotFound, "no such notice")
	case errors.Is(err, ErrEnded):
		reply.Fail(w, http.StatusConflict, "that notice has already ended: make a new one")
	default:
		s.Log.Error("Notifications - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
	}
	return true
}

// note records an event for the admin app: "maintenance notice 4 published: Back at 3am".
func (s *Service) note(kind string, n Notice, what string) {
	extra := ""
	if n.Blocking {
		extra = ", stops new orders"
	}
	s.Events.Record(platform.LevelInfo, kind, "", fmt.Sprintf("%s notice %d %s: %s%s", n.Kind, n.ID, what, n.Title, extra),
		map[string]any{"notice_id": n.ID, "blocking": n.Blocking})
}
