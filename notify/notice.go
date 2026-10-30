package notify

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"Bills-Backend/platform"
)

// Kinds of notice. The kind tells an app how to dress it (a calm card, a warning, a maintenance screen).
const (
	KindInfo        = "info"
	KindWarning     = "warning"
	KindMaintenance = "maintenance"
)

// Notice is one announcement.
type Notice struct {
	ID          int64
	Kind        string
	Lang        string
	Title       string
	Body        string
	LinkURL     string
	LinkLabel   string
	StartsAt    int64 // users see it from here (unix seconds)
	EndsAt      int64 // until here; 0 means until it is ended by hand
	EndedAt     int64 // set when it was ended by hand
	Dismissible bool
	Blocking    bool
	BlockFrom   int64 // while blocking: from here; 0 means StartsAt
	BlockUntil  int64 // until here; 0 means the notice's own end
	Apps        []string
	Platforms   []string
	MinVersion  string
	MaxVersion  string
	CreatedBy   string
	CreatedAt   int64
	UpdatedAt   int64
}

// ---- when a notice shows, and when it blocks ----

// end is when the notice stops showing: the earlier of its own end and the moment it was ended by
// hand; 0 when neither is set.
func (n Notice) end() int64 {
	switch {
	case n.EndsAt == 0:
		return n.EndedAt
	case n.EndedAt == 0:
		return n.EndsAt
	}
	return min(n.EndsAt, n.EndedAt)
}

// showingAt says whether users see it at t.
func (n Notice) showingAt(t int64) bool {
	end := n.end()
	return n.StartsAt <= t && (end == 0 || t < end)
}

// blockWindow is when new orders are refused while the notice is showing: BlockFrom (or the start)
// until BlockUntil (or the end, which also stops at once when the notice is ended by hand).
func (n Notice) blockWindow() (from, until int64) {
	from = n.BlockFrom
	if from == 0 {
		from = n.StartsAt
	}
	until = n.BlockUntil
	if until == 0 {
		until = n.end()
	} else if e := n.EndedAt; e != 0 {
		until = min(until, e)
	}
	return from, until
}

// blockingAt says whether the notice is stopping new orders at t.
func (n Notice) blockingAt(t int64) bool {
	if !n.Blocking || !n.showingAt(t) {
		return false
	}
	from, until := n.blockWindow()
	return from <= t && (until == 0 || t < until)
}

// status is where the notice is in its life at t: upcoming, active or ended.
func (n Notice) status(t int64) string {
	switch {
	case n.StartsAt > t:
		return "upcoming"
	case n.showingAt(t):
		return "active"
	}
	return "ended"
}

// ---- who a notice is for ----

// matches says whether the notice applies to the caller. Empty lists mean everyone. The app and
// platform must be named by the caller to match a notice that names them (an unnamed caller sees
// only the notices for everyone), so a notice for the dashboard is never shown to a customer's app
// by accident. An unknown app version counts as the oldest possible, so "everything older than
// 1.4" reaches old apps that do not report a version yet.
func (n Notice) matches(c platform.Caller) bool {
	if len(n.Apps) > 0 && !contains(n.Apps, c.App) {
		return false
	}
	if len(n.Platforms) > 0 && !contains(n.Platforms, c.Platform) {
		return false
	}
	if n.MinVersion != "" && compareVersions(c.Version, n.MinVersion) < 0 {
		return false
	}
	if n.MaxVersion != "" && compareVersions(c.Version, n.MaxVersion) > 0 {
		return false
	}
	return true
}

func contains(list []string, s string) bool {
	if s == "" {
		return false
	}
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var versionRule = regexp.MustCompile(`^v?\d+(\.\d+){0,3}([+-][0-9A-Za-z.+-]*)?$`)

// parseVersion reads "1.4.2", "1.4.2+7" or "v1.4": up to four numbers, anything after a + or - ignored.
// An empty or unreadable version is 0.0.0.0.
func parseVersion(v string) [4]int {
	var out [4]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	for i, part := range strings.Split(v, ".") {
		if i >= len(out) {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return [4]int{}
		}
		out[i] = n
	}
	return out
}

// compareVersions is -1, 0 or 1 as a is older than, the same as or newer than b.
func compareVersions(a, b string) int {
	x, y := parseVersion(a), parseVersion(b)
	for i := range x {
		switch {
		case x[i] < y[i]:
			return -1
		case x[i] > y[i]:
			return 1
		}
	}
	return 0
}

// ---- what an admin sends ----

// When is a time in a request: unix seconds, an RFC 3339 time ("2026-11-03T01:00:00+01:00") or a day
// ("2026-11-03", midnight Nigerian time). Empty or null is 0.
type When int64

var lagos = time.FixedZone("WAT", 3600)

func (w *When) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		*w = 0
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*w = When(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return fmt.Errorf("a time must be unix seconds, an RFC 3339 time or a day like 2026-11-03")
	}
	if t, err := time.Parse(time.RFC3339, str); err == nil {
		*w = When(t.Unix())
		return nil
	}
	if t, err := time.ParseInLocation("2006-01-02", str, lagos); err == nil {
		*w = When(t.Unix())
		return nil
	}
	return fmt.Errorf("a time must be unix seconds, an RFC 3339 time or a day like 2026-11-03")
}

// Input is the body of a request to make or change a notice.
type Input struct {
	Kind        string   `json:"kind"`
	Lang        string   `json:"lang"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	LinkURL     string   `json:"link_url"`
	LinkLabel   string   `json:"link_label"`
	StartsAt    When     `json:"starts_at"`
	EndsAt      When     `json:"ends_at"`
	Dismissible *bool    `json:"dismissible"`
	Blocking    bool     `json:"blocking"`
	BlockFrom   When     `json:"block_from"`
	BlockUntil  When     `json:"block_until"`
	Apps        []string `json:"apps"`
	Platforms   []string `json:"platforms"`
	MinVersion  string   `json:"min_version"`
	MaxVersion  string   `json:"max_version"`
}

const (
	titleMax = 80
	bodyMax  = 1000
	labelMax = 30
	urlMax   = 300
)

var nameRule = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,39}$`)

func bad(format string, a ...any) error {
	return platform.InvalidError{Message: fmt.Sprintf(format, a...)}
}

// build checks a request and turns it into a notice, with defaults filled in (shown from now, can be
// dismissed unless it blocks). now is the time the request is made.
func (in Input) build(now time.Time) (Notice, error) {
	n := Notice{
		Kind: strings.ToLower(strings.TrimSpace(in.Kind)), Lang: strings.ToLower(strings.TrimSpace(in.Lang)),
		Title: strings.TrimSpace(in.Title), Body: strings.TrimSpace(in.Body),
		LinkURL: strings.TrimSpace(in.LinkURL), LinkLabel: strings.TrimSpace(in.LinkLabel),
		StartsAt: int64(in.StartsAt), EndsAt: int64(in.EndsAt), Blocking: in.Blocking,
		BlockFrom: int64(in.BlockFrom), BlockUntil: int64(in.BlockUntil),
		MinVersion: strings.TrimSpace(in.MinVersion), MaxVersion: strings.TrimSpace(in.MaxVersion),
	}
	if n.Lang == "" {
		n.Lang = "en"
	}
	switch {
	case n.Kind != KindInfo && n.Kind != KindWarning && n.Kind != KindMaintenance:
		return n, bad(`kind must be "info", "warning" or "maintenance"`)
	case n.Lang != "en":
		return n, bad(`lang must be "en": English is the only language so far`)
	case n.Title == "" || utf8.RuneCountInString(n.Title) > titleMax:
		return n, bad("title is required, at most %d characters", titleMax)
	case utf8.RuneCountInString(n.Body) > bodyMax:
		return n, bad("body must be at most %d characters", bodyMax)
	}
	if n.LinkURL != "" {
		u, err := url.Parse(n.LinkURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(n.LinkURL) > urlMax {
			return n, bad("link_url must be an https address of at most %d characters", urlMax)
		}
		if n.LinkLabel == "" || utf8.RuneCountInString(n.LinkLabel) > labelMax {
			return n, bad("a link needs a link_label of at most %d characters", labelMax)
		}
	} else if n.LinkLabel != "" {
		return n, bad("link_label needs a link_url")
	}

	if n.StartsAt == 0 {
		n.StartsAt = now.Unix()
	}
	if n.EndsAt != 0 && n.EndsAt <= n.StartsAt {
		return n, bad("ends_at must be after starts_at")
	}
	n.Dismissible = !n.Blocking // a notice that blocks orders cannot be dismissed, unless said otherwise
	if in.Dismissible != nil {
		n.Dismissible = *in.Dismissible && !n.Blocking
	}
	if !n.Blocking && (n.BlockFrom != 0 || n.BlockUntil != 0) {
		return n, bad("block_from and block_until only apply when blocking is true")
	}
	if n.BlockFrom != 0 && n.BlockFrom < n.StartsAt {
		return n, bad("block_from cannot be before starts_at")
	}
	if n.BlockFrom != 0 && n.BlockUntil != 0 && n.BlockUntil <= n.BlockFrom {
		return n, bad("block_until must be after block_from")
	}
	if n.BlockUntil != 0 && n.BlockUntil <= n.StartsAt {
		return n, bad("block_until must be after starts_at")
	}

	var err error
	if n.Apps, err = cleanNames("apps", in.Apps); err != nil {
		return n, err
	}
	if n.Platforms, err = cleanNames("platforms", in.Platforms); err != nil {
		return n, err
	}
	for _, v := range []struct{ field, value string }{{"min_version", n.MinVersion}, {"max_version", n.MaxVersion}} {
		if v.value != "" && !versionRule.MatchString(v.value) {
			return n, bad("%s must look like 1.4.2", v.field)
		}
	}
	if n.MinVersion != "" && n.MaxVersion != "" && compareVersions(n.MinVersion, n.MaxVersion) > 0 {
		return n, bad("min_version is newer than max_version, so no app could match")
	}
	return n, nil
}

// cleanNames lower cases, checks and de-duplicates a list of app or platform names.
func cleanNames(field string, in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range in {
		name := strings.ToLower(strings.TrimSpace(raw))
		if !nameRule.MatchString(name) {
			return nil, bad("%s: %q is not a valid name (letters, digits, dots, dashes and underscores)", field, raw)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) > 10 {
		return nil, bad("%s: at most 10 names", field)
	}
	return out, nil
}
