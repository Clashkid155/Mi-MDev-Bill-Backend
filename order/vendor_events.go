package order

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"Bills-Backend/abs"
)

// ABS's Direct API gives no id for a purchase and ignores the request_id we send, so a webhook cannot
// be tied to an order by an id. It is tied by what it says instead: who it was for and what ABS charged
// us (our cost_kobo), among the orders that reached ABS recently.
//
// The rule is deliberately strict, because a wrong match would mark someone's order delivered or
// refunded: the webhook is acted on only when it fits exactly one order that no other webhook has
// claimed. Anything else (no order, several orders, an order in a state that contradicts it) is left
// alone and recorded as an event for the owner.

// vendorMatchWindow is how far back an order can have been made for a webhook to belong to it. The slowest
// settlement seen took about 45 minutes and ABS says it waits about a minute, so this leaves a wide margin;
// a longer window only makes it likelier that an unclaimed older order with the same recipient and cost
// turns a webhook ambiguous. Anything later than this is closed by hand with the resolve action.
const vendorMatchWindow = 6 * time.Hour

// ErrStillVending means the order the webhook belongs to is being bought right now: its own outcome is
// about to be recorded, so the webhook is asked to come back (ABS retries a delivery that fails).
var ErrStillVending = errors.New("order: the purchase is still in progress")

// vendedStatuses are the states of an order that has been sent to ABS (or was settled as if it had).
var vendedStatuses = []Status{StatusVending, StatusPending, StatusSuccess, StatusFailed, StatusRefundDue, StatusRefunded}

// SettleFromABS acts on one ABS webhook: a transaction.settled or transaction.failed message settles the
// PENDING order it belongs to, exactly like the owner's resolve action does. Every other message is
// ignored. A nil error does not mean an order changed; see the rule above. A non-nil error means the
// webhook should be delivered again.
func (s *Service) SettleFromABS(ctx context.Context, ev abs.Event) error {
	var delivered bool
	switch {
	case ev.Name == "transaction.settled" && ev.Status == "1":
		delivered = true
	case ev.Name == "transaction.failed" && ev.Status == "3":
	case ev.Name == "transaction.settled" || ev.Name == "transaction.failed":
		s.event(LevelWarn, "abs.unclear", "", fmt.Sprintf("ABS sent %s with status %q, which does not say what happened; nothing was changed", ev.Name, ev.Status), map[string]any{"abs_order": ev.OrderID})
		return nil
	default:
		return nil // refunds, wallet movements: nothing to settle
	}

	cost, ok := koboOf(ev.Amount)
	recipient := normalRecipient(ev.Recipient)
	if !ok || recipient == "" || ev.OrderID == "" {
		s.event(LevelWarn, "abs.unclear", "", fmt.Sprintf("ABS sent %s without a usable amount or recipient (amount %q); nothing was changed", ev.Name, ev.Amount), map[string]any{"abs_order": ev.OrderID})
		return nil
	}

	found, err := s.Store.VendorCandidates(ctx, ev.OrderID, recipient, cost, time.Now().Add(-vendorMatchWindow))
	if err != nil {
		return err
	}
	// An order that already carries this ABS id is the one (this is a repeated delivery).
	var match *Order
	for i := range found {
		if found[i].VendorRef == ev.OrderID {
			match, found = &found[i], nil
			break
		}
	}
	if match == nil {
		switch len(found) {
		case 0:
			s.event(LevelInfo, "abs.unmatched", "", fmt.Sprintf("ABS %s for %s matches none of our recent orders: a purchase made outside this server, or an order too old", ev.Name, formatNaira(cost)), map[string]any{"abs_order": ev.OrderID})
			return nil
		case 1:
			match = &found[0]
		default:
			ids := make([]string, len(found))
			for i, o := range found {
				ids[i] = o.ID
			}
			s.event(LevelWarn, "abs.ambiguous", "", fmt.Sprintf("ABS %s for %s fits %d orders (%s), so none was changed: check ABS's dashboard (see abs_order) and use the resolve action on the right one",
				ev.Name, formatNaira(cost), len(found), strings.Join(ids, ", ")), map[string]any{"abs_order": ev.OrderID, "orders": ids})
			return nil
		}
		if match.Status == StatusVending {
			return ErrStillVending
		}
		claimed, err := s.Store.ClaimVendorRef(ctx, match.ID, ev.OrderID)
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("order %s was claimed by another webhook just now", match.ID) // ABS delivers it again
		}
		match.VendorRef = ev.OrderID
	}

	return s.applyABSOutcome(ctx, *match, ev, delivered)
}

// applyABSOutcome records what ABS says became of the order it was matched to.
func (s *Service) applyABSOutcome(ctx context.Context, o Order, ev abs.Event, delivered bool) error {
	note := "ABS says: " + shortText(ev.Message, 200)
	switch {
	case o.Status == StatusPending && delivered:
		_, err := s.Resolve(ctx, o.ID, OutcomeDelivered, note, "ABS webhook")
		return ignoreSettled(err)
	case o.Status == StatusPending:
		resolved, err := s.Resolve(ctx, o.ID, OutcomeFailed, note, "ABS webhook")
		if err != nil {
			return ignoreSettled(err)
		}
		s.alertRefundDue(resolved, "ABS reported that it did not deliver ("+shortText(ev.Message, 200)+")")
		return nil
	case delivered && o.Status != StatusSuccess:
		// Recorded as failed (or already refunded) but ABS says the customer got it.
		s.event(LevelError, "abs.conflict", o.ID, fmt.Sprintf("ABS says this order was DELIVERED but it is %s: do not refund it, or the customer is paid twice. Nothing was changed", o.Status), map[string]any{"abs_order": ev.OrderID})
		s.alert("abs-conflict:"+o.ID, refundThrottle, "ABS delivered an order recorded as "+string(o.Status)+": "+o.ID,
			summary(o)+"\nABS "+ev.OrderID+" says this purchase was delivered, but the order is "+string(o.Status)+". If a refund is still due, do not send it. Nothing was changed; check ABS's dashboard.\n")
	case !delivered && o.Status == StatusSuccess:
		s.event(LevelError, "abs.conflict", o.ID, "ABS says this order FAILED but it is SUCCESS: the customer may not have received it. Nothing was changed", map[string]any{"abs_order": ev.OrderID})
		s.alert("abs-conflict:"+o.ID, refundThrottle, "ABS says a delivered order failed: "+o.ID,
			summary(o)+"\nABS "+ev.OrderID+" says this purchase failed, but the order is SUCCESS and the customer was sent a receipt. Nothing was changed; check ABS's dashboard.\n")
	default:
		s.Log.Info("Webhook - ABS", "detail", fmt.Sprintf("ABS %s confirms order %s (%s)", ev.OrderID, o.ID, o.Status))
	}
	return nil
}

// ignoreSettled treats "someone settled it first" as success: the order is where the webhook wanted it.
func ignoreSettled(err error) error {
	if errors.Is(err, ErrNotPending) {
		return nil
	}
	return err
}

// VendorCandidates returns the orders an ABS webhook could belong to: the one that already holds ref, and
// every order made since `since` for this recipient and cost that reached ABS and has no ABS id yet.
func (s *Store) VendorCandidates(ctx context.Context, ref, recipient string, costKobo int64, since time.Time) ([]Order, error) {
	args := []any{ref, recipient, costKobo, since.Unix()}
	for _, st := range vendedStatuses {
		args = append(args, string(st))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE vendor_ref = ? OR (vendor_ref = '' AND recipient = ? AND cost_kobo = ? AND created_at >= ?
			AND status IN (?`+strings.Repeat(",?", len(vendedStatuses)-1)+`))
		ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// ClaimVendorRef records ABS's id on an order that has none. Only one webhook can win it.
func (s *Store) ClaimVendorRef(ctx context.Context, id, ref string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET vendor_ref = ? WHERE id = ? AND vendor_ref = ''`, ref, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// koboOf reads an amount ABS sends as a string of naira ("99", "99.00").
func koboOf(amount string) (int64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(amount), 64)
	if err != nil || math.IsNaN(f) || f <= 0 || f > 1e9 {
		return 0, false
	}
	return int64(math.Round(f * 100)), true
}

// normalRecipient writes a phone number the way orders store it (0803...): ABS may send +234803... or 234803...
func normalRecipient(r string) string {
	r = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(r), " ", ""), "+")
	if strings.HasPrefix(r, "234") && len(r) == 13 {
		return "0" + r[3:]
	}
	return r
}

func shortText(s string, max int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}
