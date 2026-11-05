package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"Bills-Backend/reply"
)

// ProviderHealth measures the outside services the backend depends on (the payment provider, ABS,
// the mail service) from the calls the backend really makes to them. A provider's own status page
// only reports broad outages; this sees what our requests actually got back, so a provider that
// answers 500 to one kind of request shows up here even when its status page says everything is up.
//
// A call counts as failed when there was no answer (timeout, connection error), the provider
// answered 5xx, or it answered 429 (it is limiting us). Any other answer, 4xx included, counts as
// the provider working: the request was refused, but the provider was there. A call we cancelled
// ourselves is not counted either way.
//
// Counts are kept per hour in the database (so history survives a restart) and written once a
// minute. A provider that fails 3 calls in a row over at least a minute is "down": an event is
// recorded and the owner is emailed, once an hour at most per provider. The first success after
// that records a "recovered" event.
//
// Only providers that are called show up. ABS and the payment provider are also called on a timer by
// the balance check, which keeps their picture fresh when no customer is buying.
type ProviderHealth struct {
	store  *Store
	log    *slog.Logger
	events *EventRecorder // nil records nothing
	alerts Alerter        // nil emails nothing
	now    func() time.Time

	mu    sync.Mutex
	state map[string]*providerState
	names []string // in the order they were registered
}

const (
	recentCalls    = 20              // how many recent calls decide "degraded"
	downAfterCalls = 3               // consecutive failures that make a provider down...
	downAfterTime  = time.Minute     // ...when they span at least this long
	slowAfter      = 5 * time.Second // a call this slow counts as slow (it still worked)
	idleAfter      = 24 * time.Hour  // no call for this long: nothing to say about it
	statsKeep      = 90 * 24 * time.Hour
	errorMax       = 200
)

type providerState struct {
	recent      [recentCalls]bool // true = failed; a ring
	seen, next  int
	consecutive int
	streakStart time.Time
	down        bool
	lastCall    time.Time
	lastOK      time.Time
	pending     map[int64]*hourCount // not yet written, by hour
}

type hourCount struct {
	calls, failures, slow, totalMs int64
	lastError                      string
	lastErrorAt                    int64
}

// NewProviderHealth makes the tracker. events and alerts may be nil.
func NewProviderHealth(store *Store, log *slog.Logger, events *EventRecorder, alerts Alerter) *ProviderHealth {
	return &ProviderHealth{store: store, log: log, events: events, alerts: alerts, now: time.Now, state: map[string]*providerState{}}
}

// SetAlerter sets who is emailed when a provider goes down. It can be called once the mailer exists,
// which is after the clients that report to this tracker are built.
func (h *ProviderHealth) SetAlerter(a Alerter) {
	h.mu.Lock()
	h.alerts = a
	h.mu.Unlock()
}

// Observer returns the function a client calls after every request it makes to the provider called
// name ("kora", "abs"...). It is safe to call from any goroutine and never blocks on the database.
func (h *ProviderHealth) Observer(name string) func(op string, status int, err error, took time.Duration) {
	h.mu.Lock()
	if _, ok := h.state[name]; !ok {
		h.state[name] = &providerState{pending: map[int64]*hourCount{}}
		h.names = append(h.names, name)
	}
	h.mu.Unlock()
	return func(op string, status int, err error, took time.Duration) { h.record(name, op, status, err, took) }
}

func (h *ProviderHealth) record(name, op string, status int, err error, took time.Duration) {
	if errors.Is(err, context.Canceled) {
		return // we gave up, the provider did nothing wrong
	}
	failed := err != nil || status >= 500 || status == http.StatusTooManyRequests
	var why string
	if failed {
		if err != nil {
			why = op + ": " + err.Error()
		} else {
			why = fmt.Sprintf("%s: HTTP %d", op, status)
		}
		why = truncate(redact(why), errorMax)
	}

	now := h.now()
	h.mu.Lock()
	st := h.state[name]
	hour := now.Unix() / 3600 * 3600
	c := st.pending[hour]
	if c == nil {
		c = &hourCount{}
		st.pending[hour] = c
	}
	c.calls++
	c.totalMs += took.Milliseconds()
	if took >= slowAfter {
		c.slow++
	}
	st.lastCall = now
	st.recent[st.next] = failed
	st.next = (st.next + 1) % recentCalls
	st.seen = min(st.seen+1, recentCalls)

	var wentDown, recovered bool
	var streak time.Duration
	var count int
	if failed {
		c.failures++
		c.lastError, c.lastErrorAt = why, now.Unix()
		if st.consecutive == 0 {
			st.streakStart = now
		}
		st.consecutive++
		streak, count = now.Sub(st.streakStart), st.consecutive
		if !st.down && count >= downAfterCalls && streak >= downAfterTime {
			st.down, wentDown = true, true
		}
	} else {
		st.lastOK = now
		st.consecutive = 0
		if st.down {
			st.down, recovered = false, true
		}
	}
	alerts := h.alerts
	h.mu.Unlock()

	switch {
	case wentDown:
		msg := fmt.Sprintf("%s is failing: %d calls in a row over %s, the last was %s", name, count, streak.Round(time.Second), why)
		h.events.Record(LevelError, "provider.down", "", msg, map[string]any{"provider": name, "failures": count})
		if alerts != nil {
			alerts.Alert("provider-down:"+name, time.Hour, name+" is failing",
				msg+".\n\nThis comes from the calls the server makes, so it can show a problem the provider's own status page does not. "+
					"Orders that depend on it will wait and are retried; nothing is lost. GET /admin/providers shows the detail, and the server's journal has each failed call.")
		}
	case recovered:
		h.events.Record(LevelInfo, "provider.recovered", "", name+" is answering again", map[string]any{"provider": name})
	}
}

// flush writes the counts not yet stored. On failure they are kept for the next try.
func (h *ProviderHealth) flush(ctx context.Context) {
	type row struct {
		name string
		hour int64
		c    hourCount
	}
	h.mu.Lock()
	var rows []row
	for name, st := range h.state {
		for hour, c := range st.pending {
			rows = append(rows, row{name, hour, *c})
		}
		st.pending = map[int64]*hourCount{}
	}
	h.mu.Unlock()

	for _, r := range rows {
		_, err := h.store.db.ExecContext(ctx, `INSERT INTO provider_stats (provider, hour, calls, failures, slow, total_ms, last_error, last_error_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (provider, hour) DO UPDATE SET
				calls = calls + excluded.calls, failures = failures + excluded.failures,
				slow = slow + excluded.slow, total_ms = total_ms + excluded.total_ms,
				last_error = CASE WHEN excluded.last_error_at >= last_error_at THEN excluded.last_error ELSE last_error END,
				last_error_at = MAX(last_error_at, excluded.last_error_at)`,
			r.name, r.hour, r.c.calls, r.c.failures, r.c.slow, r.c.totalMs, r.c.lastError, r.c.lastErrorAt)
		if err != nil {
			if ctx.Err() == nil {
				h.log.Error("Providers - Store", "detail", "could not save provider counts: "+err.Error())
			}
			h.putBack(r.name, r.hour, r.c)
		}
	}
}

func (h *ProviderHealth) putBack(name string, hour int64, c hourCount) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.state[name].pending[hour]
	if cur == nil {
		h.state[name].pending[hour] = &c
		return
	}
	cur.calls += c.calls
	cur.failures += c.failures
	cur.slow += c.slow
	cur.totalMs += c.totalMs
	if c.lastErrorAt >= cur.lastErrorAt {
		cur.lastError, cur.lastErrorAt = c.lastError, c.lastErrorAt
	}
}

// Run writes the counts once a minute and deletes old ones daily, until ctx ends. Call Close
// after it, before the database is closed.
func (h *ProviderHealth) Run(ctx context.Context) {
	purge := func() {
		if _, err := h.store.db.ExecContext(ctx, `DELETE FROM provider_stats WHERE hour < ?`, h.now().Add(-statsKeep).Unix()); err != nil && ctx.Err() == nil {
			h.log.Error("Providers - Store", "detail", "could not purge old provider counts: "+err.Error())
		}
	}
	purge()
	flushTick, purgeTick := time.NewTicker(time.Minute), time.NewTicker(24*time.Hour)
	defer flushTick.Stop()
	defer purgeTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTick.C:
			h.flush(ctx)
		case <-purgeTick.C:
			purge()
		}
	}
}

// Close writes whatever is still waiting.
func (h *ProviderHealth) Close() { h.flush(context.Background()) }

// ---- reading ----

// ProviderHour is one hour of a provider's calls.
type ProviderHour struct {
	Hour     int64 `json:"hour"` // unix seconds, the start of the hour
	Calls    int64 `json:"calls"`
	Failures int64 `json:"failures"`
	AvgMs    int64 `json:"avg_ms"`
}

// ProviderReport is what the admin app shows for one provider.
type ProviderReport struct {
	Name                string         `json:"name"`
	State               string         `json:"state"` // healthy, degraded, down or idle
	Calls               int64          `json:"calls"`
	Failures            int64          `json:"failures"`
	SuccessPercent      *float64       `json:"success_percent"` // null when there were no calls
	SlowCalls           int64          `json:"slow_calls"`
	AvgMs               int64          `json:"avg_ms"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	LastCallAt          int64          `json:"last_call_at"` // since the server started; 0 if none yet
	LastOKAt            int64          `json:"last_ok_at"`   // since the server started; 0 if none yet
	LastFailure         string         `json:"last_failure"` // what the last failed call was, "" if none in the window
	LastFailureAt       int64          `json:"last_failure_at"`
	Hours               []ProviderHour `json:"hours"`
}

// Report is the state of every provider over the last hours hours (at most a week is sensible).
func (h *ProviderHealth) Report(ctx context.Context, hours int) ([]ProviderReport, error) {
	h.flush(ctx)
	now := h.now()
	since := now.Add(-time.Duration(hours)*time.Hour).Unix() / 3600 * 3600

	h.mu.Lock()
	names := append([]string(nil), h.names...)
	type live struct {
		consecutive          int
		down                 bool
		lastCall, lastOK     time.Time
		recentFails, recentN int
	}
	snap := map[string]live{}
	for _, n := range names {
		st := h.state[n]
		l := live{consecutive: st.consecutive, down: st.down, lastCall: st.lastCall, lastOK: st.lastOK, recentN: st.seen}
		for i := 0; i < st.seen; i++ {
			if st.recent[i] {
				l.recentFails++
			}
		}
		snap[n] = l
	}
	h.mu.Unlock()

	out := make([]ProviderReport, 0, len(names))
	for _, n := range names {
		r := ProviderReport{Name: n, Hours: []ProviderHour{}}
		rows, err := h.store.db.QueryContext(ctx, `SELECT hour, calls, failures, slow, total_ms, last_error, last_error_at
			FROM provider_stats WHERE provider = ? AND hour >= ? ORDER BY hour`, n, since)
		if err != nil {
			return nil, err
		}
		var totalMs int64
		for rows.Next() {
			var hr ProviderHour
			var slow, ms, errAt int64
			var msg string
			if err := rows.Scan(&hr.Hour, &hr.Calls, &hr.Failures, &slow, &ms, &msg, &errAt); err != nil {
				rows.Close()
				return nil, err
			}
			if hr.Calls > 0 {
				hr.AvgMs = ms / hr.Calls
			}
			r.Calls += hr.Calls
			r.Failures += hr.Failures
			r.SlowCalls += slow
			totalMs += ms
			if errAt > r.LastFailureAt {
				r.LastFailure, r.LastFailureAt = msg, errAt
			}
			r.Hours = append(r.Hours, hr)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if r.Calls > 0 {
			p := float64(r.Calls-r.Failures) / float64(r.Calls) * 100
			r.SuccessPercent = &p
			r.AvgMs = totalMs / r.Calls
		}
		l := snap[n]
		r.ConsecutiveFailures = l.consecutive
		if !l.lastCall.IsZero() {
			r.LastCallAt = l.lastCall.Unix()
		}
		if !l.lastOK.IsZero() {
			r.LastOKAt = l.lastOK.Unix()
		}
		// Not called since the server started and nothing stored, or not called for a day: nothing to say.
		idle := (l.lastCall.IsZero() && r.Calls == 0) || (!l.lastCall.IsZero() && now.Sub(l.lastCall) > idleAfter)
		switch {
		case l.down:
			r.State = "down"
		case idle:
			r.State = "idle"
		case l.consecutive > 0, l.recentFails >= 2 && l.recentFails*5 >= l.recentN:
			r.State = "degraded"
		default:
			r.State = "healthy"
		}
		out = append(out, r)
	}
	return out, nil
}

// providers answers GET /admin/providers?hours=24 for any admin token.
func (a *Admin) providers(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 168 {
			reply.Fail(w, http.StatusBadRequest, "hours must be between 1 and 168")
			return
		}
		hours = n
	}
	report, err := a.Providers.Report(r.Context(), hours)
	if err != nil {
		a.Log.Error("Providers - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	reply.OK(w, http.StatusOK, map[string]any{"hours": hours, "providers": report})
}
