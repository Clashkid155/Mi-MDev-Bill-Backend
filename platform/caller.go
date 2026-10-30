package platform

import (
	"context"
	"net/http"
	"strings"
)

// Caller is which app is asking: its name, platform and version, as the app says. The app sends
// them as headers on every request (X-App, X-Platform, X-App-Version); a query string with app,
// platform and version works too, for a plain GET. They are self reported, which is fine for notices
// and for telling an old app to update: it is not security, and a caller can lie.
type Caller struct {
	App      string // "mi-mdev", "dashboard": a fixed name each app ships with; lower case
	Platform string // "android", "ios", "web": lower case
	Version  string // "1.4.2" (a build suffix such as "+7" is allowed and ignored); empty when unknown
}

// CallerFrom reads who is calling from the request. Anything missing is left empty.
func CallerFrom(r *http.Request) Caller {
	pick := func(header, query string, max int) string {
		v := strings.TrimSpace(r.Header.Get(header))
		if v == "" {
			v = strings.TrimSpace(r.URL.Query().Get(query))
		}
		if len(v) > max {
			v = v[:max]
		}
		return v
	}
	return Caller{
		App:      strings.ToLower(pick("X-App", "app", 40)),
		Platform: strings.ToLower(pick("X-Platform", "platform", 20)),
		Version:  pick("X-App-Version", "version", 30),
	}
}

// Block says why something is stopped for now, in words a customer can read.
type Block struct {
	NoticeID int64  // the notice that stops it
	Title    string // short, for a heading
	Message  string // the notice's text
	Until    int64  // unix seconds when it ends; 0 when it ends when someone ends it
}

// Gate lets a feature ask "is this stopped right now for this caller?" without knowing why.
// Maintenance notices implement it; the bills code only sees this interface, so it does not
// depend on notifications.
type Gate interface {
	// OrdersBlocked says whether placing a new order is stopped for the caller.
	OrdersBlocked(ctx context.Context, c Caller) (Block, bool)
}
