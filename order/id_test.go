package order

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestNewIDShapeAndSpread(t *testing.T) {
	shape := regexp.MustCompile(`^MDEV_[0-9A-HJKMNP-TV-Z]{16}$`)
	seen := map[string]bool{}
	var perPosition [16][32]int
	const n = 200000
	for i := 0; i < n; i++ {
		id := newID()
		if !shape.MatchString(id) {
			t.Fatalf("id %q is not MDEV_ plus 16 characters from the safe alphabet", id)
		}
		if seen[id] {
			t.Fatalf("id %q repeated after %d ids", id, i)
		}
		seen[id] = true
		for p := 0; p < 16; p++ {
			for sym := 0; sym < 32; sym++ {
				if id[5+p] == idAlphabet[sym] {
					perPosition[p][sym]++
				}
			}
		}
	}
	// Every symbol should show up about n/32 = 6250 times in every position; a
	// biased generator would drift far outside this wide band.
	for p := range perPosition {
		for sym, count := range perPosition[p] {
			if count < 5600 || count > 6900 {
				t.Fatalf("position %d symbol %c appeared %d times in %d ids, expected about 6250", p, idAlphabet[sym], count, n)
			}
		}
	}
}

// A clash on the id must never lose the order: Insert draws a new id.
func TestInsertRetriesWhenTheIDIsTaken(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "ids.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	defer func(old func() string) { idGen = old }(idGen)

	ids := []string{"MDEV_SAMEID", "MDEV_SAMEID", "MDEV_SAMEID", "MDEV_FRESHID", "MDEV_THIRDID"}
	next := 0
	idGen = func() string { id := ids[next]; next++; return id }

	make1 := func(key string) Order {
		return Order{IdempotencyKey: key, Kind: KindAirtime, Network: "N", Recipient: "08012345678", AmountKobo: 100_00,
			CustomerEmail: "a@b.co", ExpiresAt: time.Now().Add(time.Hour)}
	}
	first, second := make1("k1"), make1("k2")
	if err := store.Insert(context.Background(), &first); err != nil || first.ID != "MDEV_SAMEID" {
		t.Fatalf("first insert: id %q, err %v", first.ID, err)
	}
	if err := store.Insert(context.Background(), &second); err != nil || second.ID != "MDEV_FRESHID" {
		t.Fatalf("second insert should retry past the clash: id %q, err %v", second.ID, err)
	}
	if got, err := store.ByID(context.Background(), "MDEV_SAMEID"); err != nil || got.IdempotencyKey != "k1" {
		t.Fatalf("the first order must be untouched: %+v, %v", got, err)
	}

	// A repeated idempotency key is not an id clash and is not retried.
	next = 4 // an unused id, so the only problem left is the repeated key
	dup := make1("k1")
	if err := store.Insert(context.Background(), &dup); err == nil {
		t.Fatal("a repeated idempotency key must still be an error")
	}
}
