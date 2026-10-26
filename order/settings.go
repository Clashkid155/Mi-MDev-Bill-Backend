package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Settings are the few things the owner may change while the server runs, from
// the admin API, without a restart. Each one has a value from .env (or a
// built-in default) and may be overridden here; deleting the override puts the
// .env value back. The override lives in the database, so it survives a restart.
//
// Never settable here: keys, secrets, the admin token, the payment provider,
// ABS_SANDBOX (whether purchases use real money), APP_ENV, the database path and
// the port. Those stay in .env, where a wrong value cannot be typed into an API call.
//
// Every change is checked by the same rules as startup, so the API cannot be
// used to set a markup that loses money.

const (
	kindPercent = "percent" // a number of percent, 0 or more
	kindNaira   = "naira"   // whole naira, 0 or more
	kindMinutes = "minutes" // whole minutes, 1 or more
	kindList    = "list"    // names separated by commas
	kindEmail   = "email"   // one address, or empty for none
	kindBool    = "bool"    // true or false
)

type settingDef struct {
	Key         string
	Description string
	Kind        string
	// Needs names something the server must have for the setting to mean
	// anything ("mail": the email settings in .env). Without it the setting
	// cannot be changed, instead of being accepted and silently doing nothing.
	Needs string
}

var settingDefs = []settingDef{
	{"markup.mtn", "Percent added to the ABS price of MTN plans below markup.large_from_naira", kindPercent, ""},
	{"markup.mtn_large", "Percent added to MTN plans at or above markup.large_from_naira (a small markup keeps big plans at the official price)", kindPercent, ""},
	{"markup.large_from_naira", "ABS price in naira from which the large plan markup applies", kindNaira, ""},
	{"markup.airtel", "Percent added to the ABS price of Airtel plans", kindPercent, ""},
	{"markup.glo", "Percent added to the ABS price of Glo plans", kindPercent, ""},
	{"markup.9mobile", "Percent added to the ABS price of 9mobile plans", kindPercent, ""},
	{"markup.default", "Percent added to the ABS price of any other network", kindPercent, ""},
	{"hidden_plans", "Data plan ids that are not offered at all, separated by commas", kindList, ""},
	{"electricity_fee.percent", "Electricity service fee: this percent of the amount, rounded up to whole naira", kindPercent, ""},
	{"electricity_fee.min_naira", "Electricity service fee: never less than this many naira", kindNaira, ""},
	{"payment_fee_warn_percent", "Warn (log and email) when the payment provider's fee is above this percent of a payment; 0 turns it off", kindPercent, ""},
	{"alerts.email", "Where problem alerts are emailed; empty turns the emails off", kindEmail, "mail"},
	{"alerts.low_balance_naira", "Warn when the ABS wallet falls below this many naira; 0 turns the check off", kindNaira, "mail"},
	{"alerts.check_minutes", "How often the ABS wallet is checked, in minutes (takes effect after the current wait)", kindMinutes, "mail"},
	{"mail.sandbox", "true: emails (receipts and alerts) are only logged; false: they are really sent", kindBool, "mail"},
}

func findSetting(key string) (settingDef, bool) {
	for _, d := range settingDefs {
		if d.Key == key {
			return d, true
		}
	}
	return settingDef{}, false
}

var (
	// ErrUnknownSetting: no setting has this key.
	ErrUnknownSetting = errors.New("order: unknown setting")
	// ErrSettingUnavailable: the setting needs something this server was not started with.
	ErrSettingUnavailable = errors.New("order: setting not available")
)

// Settings holds the effective value of every setting. Safe for concurrent use.
type Settings struct {
	store *Store
	log   *slog.Logger

	mu        sync.RWMutex
	defaults  map[string]string
	overrides map[string]string
	caps      map[string]bool
	listeners []func()
}

// NewSettings loads the overrides saved in the database on top of defaults, the
// values from .env. An override that would not pass today's rules is ignored with
// a warning rather than stopping the server.
func NewSettings(ctx context.Context, store *Store, defaults map[string]string, log *slog.Logger) (*Settings, error) {
	s := &Settings{store: store, log: log, defaults: defaults, overrides: map[string]string{}, caps: map[string]bool{}}
	saved, err := store.loadSettings(ctx)
	if err != nil {
		return nil, err
	}
	for key, raw := range saved {
		if canonical, err := s.check(key, raw); err != nil {
			log.Warn("Settings - Order", "detail", fmt.Sprintf("ignoring the saved value %q for %s: %v", raw, key, err))
		} else {
			s.overrides[key] = canonical
		}
	}
	return s, nil
}

// SetCapability tells Settings what the server has ("mail"), so the settings
// that need it can be changed.
func (s *Settings) SetCapability(name string, available bool) {
	s.mu.Lock()
	s.caps[name] = available
	s.mu.Unlock()
}

// OnChange registers a function called after any setting changes, so things that
// copy a value (the mailer's sandbox flag, the alert address) can pick it up.
func (s *Settings) OnChange(f func()) {
	s.mu.Lock()
	s.listeners = append(s.listeners, f)
	s.mu.Unlock()
}

func (s *Settings) value(key string) string {
	if v, ok := s.overrides[key]; ok {
		return v
	}
	return s.defaults[key]
}

// Value is the effective value of a setting, as text.
func (s *Settings) Value(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value(key)
}

func (s *Settings) Float(key string) float64 { f, _ := strconv.ParseFloat(s.Value(key), 64); return f }
func (s *Settings) Int(key string) int64     { n, _ := strconv.ParseInt(s.Value(key), 10, 64); return n }
func (s *Settings) Bool(key string) bool     { return s.Value(key) == "true" }
func (s *Settings) Text(key string) string   { return s.Value(key) }

// Pricing is the pricing rules as they stand now.
func (s *Settings) Pricing() Pricing {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pricingWith(nil)
}

// pricingWith builds the pricing from the effective values, with extra applied on top.
func (s *Settings) pricingWith(extra map[string]string) Pricing {
	get := func(key string) string {
		if v, ok := extra[key]; ok {
			return v
		}
		return s.value(key)
	}
	num := func(key string) float64 { f, _ := strconv.ParseFloat(get(key), 64); return f }
	whole := func(key string) int64 { n, _ := strconv.ParseInt(get(key), 10, 64); return n }
	hidden := map[string]bool{}
	for _, id := range splitList(get("hidden_plans")) {
		hidden[id] = true
	}
	return Pricing{
		Markups: map[string]float64{
			"mtn": num("markup.mtn"), "airtel": num("markup.airtel"), "glo": num("markup.glo"), "9mobile": num("markup.9mobile"),
		},
		DefaultMarkup:         num("markup.default"),
		LargeFromKobo:         whole("markup.large_from_naira") * kobo,
		LargeMarkups:          map[string]float64{"mtn": num("markup.mtn_large")},
		Hidden:                hidden,
		ElectricityFeePercent: num("electricity_fee.percent"),
		ElectricityFeeMinKobo: whole("electricity_fee.min_naira") * kobo,
	}
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// InvalidSettingError is a value that is not allowed (HTTP 400).
type InvalidSettingError struct{ Message string }

func (e InvalidSettingError) Error() string { return e.Message }

// canonical turns what was typed into the one form that is stored, or says why not.
func canonical(def settingDef, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch def.Kind {
	case kindPercent:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v > 100 {
			return "", InvalidSettingError{def.Key + " must be a percent between 0 and 100"}
		}
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case kindNaira, kindMinutes:
		v, err := strconv.ParseInt(raw, 10, 64)
		min := int64(0)
		if def.Kind == kindMinutes {
			min = 1
		}
		if err != nil || v < min || v > 1_000_000_000 {
			return "", InvalidSettingError{fmt.Sprintf("%s must be a whole number, at least %d", def.Key, min)}
		}
		return strconv.FormatInt(v, 10), nil
	case kindBool:
		if raw != "true" && raw != "false" {
			return "", InvalidSettingError{def.Key + " must be true or false"}
		}
		return raw, nil
	case kindEmail:
		if raw == "" {
			return "", nil
		}
		addr, err := mail.ParseAddress(raw)
		if err != nil || addr.Address != raw {
			return "", InvalidSettingError{def.Key + " must be one email address like you@example.com, or empty"}
		}
		return raw, nil
	case kindList:
		seen := map[string]bool{}
		var ids []string
		for _, id := range splitList(raw) {
			if len(id) > 40 || strings.ContainsAny(id, " \t\n\"'") {
				return "", InvalidSettingError{"plan ids are short names without spaces or quotes, separated by commas"}
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if len(ids) > 200 {
			return "", InvalidSettingError{"at most 200 plan ids"}
		}
		return strings.Join(ids, ","), nil
	}
	return "", InvalidSettingError{"unsupported setting"}
}

// check validates a new value for key against everything else as it stands now.
// The caller holds no lock; check takes the read lock itself when it needs one.
func (s *Settings) check(key, raw string) (string, error) {
	def, ok := findSetting(key)
	if !ok {
		return "", ErrUnknownSetting
	}
	value, err := canonical(def, raw)
	if err != nil {
		return "", err
	}
	s.mu.RLock()
	err = s.pricingWith(map[string]string{key: value}).Validate()
	s.mu.RUnlock()
	if err != nil {
		return "", InvalidSettingError{err.Error()}
	}
	return value, nil
}

// SettingView is one setting as the admin sees it.
type SettingView struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Value       any    `json:"value"`
	Default     any    `json:"default"`
	// Source is "api" when the value was changed through the API, "default" when it is the .env (or built-in) value.
	Source string `json:"source"`
	// Available is false for a setting this server cannot use (the email ones when no email is configured).
	Available bool `json:"available"`
}

// typed shows a stored value the way JSON should carry it.
func typed(kind, stored string) any {
	switch kind {
	case kindPercent:
		f, _ := strconv.ParseFloat(stored, 64)
		return f
	case kindNaira, kindMinutes:
		n, _ := strconv.ParseInt(stored, 10, 64)
		return n
	case kindBool:
		return stored == "true"
	case kindList:
		if ids := splitList(stored); ids != nil {
			return ids
		}
		return []string{}
	}
	return stored
}

func (s *Settings) viewOf(def settingDef) SettingView {
	_, overridden := s.overrides[def.Key]
	source := "default"
	if overridden {
		source = "api"
	}
	return SettingView{
		Key: def.Key, Description: def.Description, Kind: def.Kind,
		Value: typed(def.Kind, s.value(def.Key)), Default: typed(def.Kind, s.defaults[def.Key]),
		Source: source, Available: def.Needs == "" || s.caps[def.Needs],
	}
}

// List shows every setting.
func (s *Settings) List() []SettingView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SettingView, 0, len(settingDefs))
	for _, def := range settingDefs {
		out = append(out, s.viewOf(def))
	}
	return out
}

// Set changes a setting. It returns ErrUnknownSetting, ErrSettingUnavailable or
// an InvalidSettingError when the change is not allowed, and changes nothing then.
func (s *Settings) Set(ctx context.Context, key, raw string) (SettingView, error) {
	def, ok := findSetting(key)
	if !ok {
		return SettingView{}, ErrUnknownSetting
	}
	if err := s.available(def); err != nil {
		return SettingView{}, err
	}
	value, err := s.check(key, raw)
	if err != nil {
		return SettingView{}, err
	}
	s.mu.Lock()
	old := s.value(key)
	if err := s.store.saveSetting(ctx, key, value); err != nil {
		s.mu.Unlock()
		return SettingView{}, err
	}
	s.overrides[key] = value
	s.recordChange(ctx, key, old, value)
	view := s.viewOf(def)
	listeners := append([]func(){}, s.listeners...)
	s.mu.Unlock()
	for _, f := range listeners {
		f()
	}
	return view, nil
}

// Reset removes the override, so the setting follows .env again.
func (s *Settings) Reset(ctx context.Context, key string) (SettingView, error) {
	def, ok := findSetting(key)
	if !ok {
		return SettingView{}, ErrUnknownSetting
	}
	if err := s.available(def); err != nil {
		return SettingView{}, err
	}
	s.mu.Lock()
	if _, overridden := s.overrides[key]; !overridden {
		view := s.viewOf(def)
		s.mu.Unlock()
		return view, nil
	}
	// The default has to pass today's rules too, or resetting would break the pricing.
	old := s.value(key)
	delete(s.overrides, key)
	if err := s.pricingWith(nil).Validate(); err != nil {
		s.overrides[key] = old
		s.mu.Unlock()
		return SettingView{}, InvalidSettingError{"the .env value for " + key + " is not allowed: " + err.Error()}
	}
	if err := s.store.deleteSetting(ctx, key); err != nil {
		s.overrides[key] = old
		s.mu.Unlock()
		return SettingView{}, err
	}
	s.recordChange(ctx, key, old, s.value(key))
	view := s.viewOf(def)
	listeners := append([]func(){}, s.listeners...)
	s.mu.Unlock()
	for _, f := range listeners {
		f()
	}
	return view, nil
}

func (s *Settings) available(def settingDef) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if def.Needs != "" && !s.caps[def.Needs] {
		return fmt.Errorf("%w: %s needs the email settings (ZEPTOMAIL_TOKEN and MAIL_FROM_EMAIL) in .env", ErrSettingUnavailable, def.Key)
	}
	return nil
}

// recordChange writes the audit trail and the log line. The caller holds the lock.
func (s *Settings) recordChange(ctx context.Context, key, old, now string) {
	if old == now {
		return
	}
	if err := s.store.logSettingChange(ctx, key, old, now); err != nil {
		s.log.Warn("Settings - Order", "detail", "could not record the change of "+key+": "+err.Error())
	}
	s.log.Info("Settings - Order", "detail", fmt.Sprintf("%s changed from %q to %q", key, old, now))
}

// SettingChange is one entry of the audit trail.
type SettingChange struct {
	Key       string    `json:"key"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	ChangedAt time.Time `json:"changed_at"`
}

// History lists the latest changes, newest first.
func (s *Settings) History(ctx context.Context, limit int) ([]SettingChange, error) {
	return s.store.settingsHistory(ctx, limit)
}

// ---- storage ----

func (s *Store) loadSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) saveSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, time.Now().Unix())
	return err
}

func (s *Store) deleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

func (s *Store) logSettingChange(ctx context.Context, key, old, now string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings_log (key, old_value, new_value, changed_at) VALUES (?,?,?,?)`, key, old, now, time.Now().Unix())
	return err
}

func (s *Store) settingsHistory(ctx context.Context, limit int) ([]SettingChange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, old_value, new_value, changed_at FROM settings_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SettingChange{}
	for rows.Next() {
		var c SettingChange
		var at int64
		if err := rows.Scan(&c.Key, &c.From, &c.To, &at); err != nil {
			return nil, err
		}
		c.ChangedAt = time.Unix(at, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}
