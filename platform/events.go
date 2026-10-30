package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Bills-Backend/logging"
)

// Event levels.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Event is one thing that happened, as the admin API shows it.
type Event struct {
	ID      int64           `json:"id"`
	At      int64           `json:"at"`
	Level   string          `json:"level"`
	Kind    string          `json:"kind"`
	OrderID string          `json:"order_id,omitempty"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// ---- keeping customers' details out ----

var (
	reEmail   = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	rePhone   = regexp.MustCompile(`(?:\+?234|0)[789][01]\d{8}`)
	reToken   = regexp.MustCompile(`mdev_[A-Za-z0-9_-]{8,}`)
	reBearer  = regexp.MustCompile(`(?i)bearer\s+\S+`)
	reSecret  = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)
	reGrouped = regexp.MustCompile(`\b\d{3,5}(?:[- ]\d{3,5}){3,}\b`) // an electricity token written in groups
	reLong    = regexp.MustCompile(`\b\d{9,}\b`)                     // a meter, account or phone number
	reOrderID = regexp.MustCompile(`MDEV_[0-9A-Z]{16}`)
)

// redact removes what must not be kept: email addresses, phone numbers, tokens, long secrets, and
// long runs of digits (meter and account numbers, electricity tokens). Order ids are left alone.
func redact(s string) string {
	s = reEmail.ReplaceAllString(s, "[email]")
	s = rePhone.ReplaceAllString(s, "[phone]")
	s = reToken.ReplaceAllString(s, "[token]")
	s = reBearer.ReplaceAllString(s, "Bearer [hidden]")
	s = reGrouped.ReplaceAllString(s, "[number]")
	s = reSecret.ReplaceAllStringFunc(s, func(m string) string {
		if reOrderID.MatchString(m) && len(m) < 32 {
			return m
		}
		return "[secret]"
	})
	s = reLong.ReplaceAllString(s, "[number]")
	return strings.Join(strings.Fields(s), " ")
}

// ---- the recorder ----

// EventRecorder writes events to the database in the background. Recording never waits for the
// database and never fails a caller: if events arrive faster than they can be written, the extra
// ones are dropped (a flood of identical warnings is not worth the disk).
type EventRecorder struct {
	store *Store
	log   *slog.Logger

	mu      sync.RWMutex // guards closed and the channel against a send after close
	closed  bool
	queue   chan Event
	pending atomic.Int64
	stopped chan struct{}

	dedupeMu sync.Mutex
	recent   map[string]time.Time // automatic captures by text, to drop identical repeats
	window   time.Time
	count    int
}

const (
	eventQueue       = 1000
	dedupeFor        = 60 * time.Second
	capturesPerMinut = 60
	messageMax       = 400
)

// NewEventRecorder starts the background writer.
func NewEventRecorder(store *Store, log *slog.Logger) *EventRecorder {
	r := &EventRecorder{store: store, log: log, queue: make(chan Event, eventQueue), stopped: make(chan struct{}), recent: map[string]time.Time{}}
	go r.run()
	return r
}

func (r *EventRecorder) run() {
	defer close(r.stopped)
	for e := range r.queue {
		if _, err := r.store.AddEvent(context.Background(), e); err != nil && r.log != nil {
			r.log.Warn("Events - Store", "detail", "could not save an event: "+err.Error())
		}
		r.pending.Add(-1)
	}
}

// Record notes something that happened. kind is a short dotted name (order.paid), orderID may be
// empty, message is redacted, detail is a small object of figures (amounts, never a customer's details).
func (r *EventRecorder) Record(level, kind, orderID, message string, detail map[string]any) {
	if r == nil {
		return
	}
	e := Event{At: time.Now().Unix(), Level: level, Kind: kind, OrderID: orderID, Message: truncate(redact(message), messageMax)}
	if len(detail) > 0 {
		if raw, err := json.Marshal(detail); err == nil {
			e.Detail = raw
		}
	}
	r.enqueue(e)
}

func (r *EventRecorder) enqueue(e Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return
	}
	r.pending.Add(1)
	select {
	case r.queue <- e:
	default:
		r.pending.Add(-1) // the queue is full: drop it rather than slow anything down
	}
}

// Capture is the sink for the logger: warnings and errors become events of kind "system".
// The web request lines are the access log and are left out (except a 5xx), as are the order
// flow warnings that Record already says in better words.
func (r *EventRecorder) Capture(e logging.Entry) {
	if r == nil {
		return
	}
	switch {
	case e.Tag == "Events - Store":
		return // this recorder's own trouble: recording it would only repeat it
	case e.Tag == "HTTP" && e.Level < slog.LevelError:
		return
	case e.Level == slog.LevelWarn && (e.Tag == "ConfirmPayment - Order" || e.Tag == "Vend - Order"):
		return
	}
	level := LevelWarn
	if e.Level >= slog.LevelError {
		level = LevelError
	}
	text := redact(e.Tag + ": " + e.Detail)
	if !r.admit(level + text) {
		return
	}
	orderID := reOrderID.FindString(e.Tag + " " + e.Detail)
	r.enqueue(Event{At: e.Time.Unix(), Level: level, Kind: "system", OrderID: orderID, Message: truncate(text, messageMax)})
}

// admit drops a repeat of something captured in the last minute, and caps how many are kept a minute.
func (r *EventRecorder) admit(key string) bool {
	r.dedupeMu.Lock()
	defer r.dedupeMu.Unlock()
	now := time.Now()
	if last, ok := r.recent[key]; ok && now.Sub(last) < dedupeFor {
		return false
	}
	if now.Sub(r.window) >= time.Minute {
		r.window, r.count = now, 0
		for k, t := range r.recent { // forget the old ones so the map cannot grow for ever
			if now.Sub(t) > dedupeFor {
				delete(r.recent, k)
			}
		}
	}
	if r.count >= capturesPerMinut {
		return false
	}
	r.count++
	r.recent[key] = now
	return true
}

// Flush waits (briefly) until everything recorded so far has been written. Tests and shutdown use it.
func (r *EventRecorder) Flush() {
	if r == nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.pending.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// Close writes what is queued and stops the writer. Recording after Close does nothing.
func (r *EventRecorder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	select {
	case <-r.stopped:
	case <-time.After(5 * time.Second):
	}
}

// ---- storage ----

// AddEvent saves one event.
func (s *Store) AddEvent(ctx context.Context, e Event) (int64, error) {
	if e.At == 0 {
		e.At = time.Now().Unix()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO events (at, level, kind, order_id, message, detail) VALUES (?,?,?,?,?,?)`,
		e.At, e.Level, e.Kind, e.OrderID, e.Message, string(e.Detail))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EventFilter picks which events to list.
type EventFilter struct {
	MinLevel string // "info" (everything), "warn" (warnings and errors) or "error"
	Kind     string // an exact kind, or a prefix ending in a dot: "order."
	OrderID  string
	Since    time.Time
	BeforeID int64
	Limit    int
}

var levelRank = map[string]int{LevelInfo: 0, LevelWarn: 1, LevelError: 2}

// ListEvents returns events newest first.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	query, args := `SELECT id, at, level, kind, order_id, message, detail FROM events WHERE 1=1`, []any{}
	switch f.MinLevel {
	case LevelWarn:
		query += ` AND level IN ('warn','error')`
	case LevelError:
		query += ` AND level = 'error'`
	}
	if strings.HasSuffix(f.Kind, ".") {
		query += ` AND substr(kind, 1, ?) = ?`
		args = append(args, len(f.Kind), f.Kind)
	} else if f.Kind != "" {
		query += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.OrderID != "" {
		query += ` AND order_id = ?`
		args = append(args, f.OrderID)
	}
	if !f.Since.IsZero() {
		query += ` AND at >= ?`
		args = append(args, f.Since.Unix())
	}
	if f.BeforeID > 0 {
		query += ` AND id < ?`
		args = append(args, f.BeforeID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var detail string
		if err := rows.Scan(&e.ID, &e.At, &e.Level, &e.Kind, &e.OrderID, &e.Message, &detail); err != nil {
			return nil, err
		}
		if detail != "" {
			e.Detail = json.RawMessage(detail)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventSummary counts events by level since a moment, and returns the latest error if there was one.
type EventSummary struct {
	Since       int64  `json:"since"`
	Info        int    `json:"info"`
	Warn        int    `json:"warn"`
	Error       int    `json:"error"`
	LatestError *Event `json:"latest_error,omitempty"`
}

// SummarizeEvents is the figures for a badge: how many warnings and errors lately.
func (s *Store) SummarizeEvents(ctx context.Context, since time.Time) (EventSummary, error) {
	sum := EventSummary{Since: since.Unix()}
	rows, err := s.db.QueryContext(ctx, `SELECT level, COUNT(*) FROM events WHERE at >= ? GROUP BY level`, since.Unix())
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var level string
		var n int
		if err := rows.Scan(&level, &n); err != nil {
			return sum, err
		}
		switch level {
		case LevelInfo:
			sum.Info = n
		case LevelWarn:
			sum.Warn = n
		case LevelError:
			sum.Error = n
		}
	}
	if err := rows.Err(); err != nil {
		return sum, err
	}
	var e Event
	var detail string
	err = s.db.QueryRowContext(ctx, `SELECT id, at, level, kind, order_id, message, detail FROM events WHERE level = 'error' AND at >= ? ORDER BY id DESC LIMIT 1`, since.Unix()).
		Scan(&e.ID, &e.At, &e.Level, &e.Kind, &e.OrderID, &e.Message, &detail)
	switch {
	case err == nil:
		if detail != "" {
			e.Detail = json.RawMessage(detail)
		}
		sum.LatestError = &e
	case err != sql.ErrNoRows:
		return sum, err
	}
	return sum, nil
}

// PurgeEvents deletes events older than the cutoff and says how many went.
func (s *Store) PurgeEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE at < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
