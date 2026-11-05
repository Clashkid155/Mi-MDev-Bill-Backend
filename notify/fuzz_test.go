package notify

import (
	"encoding/json"
	"testing"
	"time"
)

// What strangers and admins can send must never crash the server or let an impossible notice in.

func FuzzVersions(f *testing.F) {
	for _, s := range []string{"", "1.4.2", "v1.4.2+7", "1.4.2-beta", "99999999999999999999.1", "1..2", "-1", ".", "1.2.3.4.5", "\x00", "💥"} {
		f.Add(s, "1.0")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := compareVersions(a, b), compareVersions(b, a)
		if ab != -ba {
			t.Fatalf("compare is not symmetric: %q %q gave %d and %d", a, b, ab, ba)
		}
		if compareVersions(a, a) != 0 {
			t.Fatalf("%q is not equal to itself", a)
		}
	})
}

func FuzzAdminInput(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"kind":"info","title":"x"}`, `{"kind":"maintenance","title":"x","blocking":true,"starts_at":"2026-11-03","ends_at":1,"apps":["a"]}`,
		`{"starts_at":"nope"}`, `{"starts_at":1e400}`, `{"apps":[null]}`, `{"title":"\u0000"}`, `[]`, `null`, `{"ends_at":-5,"kind":"info","title":"x"}`,
	} {
		f.Add(s)
	}
	now := time.Unix(1_800_000_000, 0)
	f.Fuzz(func(t *testing.T, body string) {
		var in Input
		if json.Unmarshal([]byte(body), &in) != nil {
			return
		}
		n, err := in.build(now)
		if err != nil {
			return
		}
		// Whatever got through must make sense.
		if n.Title == "" || (n.EndsAt != 0 && n.EndsAt <= n.StartsAt) || (n.Blocking && n.Dismissible) || (!n.Blocking && (n.BlockFrom != 0 || n.BlockUntil != 0)) {
			t.Fatalf("an impossible notice got through: %+v from %q", n, body)
		}
		// And it must be safe to evaluate at any time.
		for _, at := range []int64{0, now.Unix(), 1 << 40} {
			n.showingAt(at)
			n.blockingAt(at)
			n.status(at)
		}
	})
}
