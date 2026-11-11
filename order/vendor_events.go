package order

import (
	"context"
	"database/sql"
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
	if state := absState(ev); state != "" {
		if alerted, err := s.trackABS(ctx, ev, state); err != nil || alerted {
			return err // a purchase ABS had refunded is settling again: the owner decides, nothing is acted on
		}
	}
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
			if delivered {
				twins, err := s.Store.VendorTwins(ctx, ev.OrderID, recipient, cost, time.Now().Add(-vendorMatchWindow))
				if err != nil {
					return err
				}
				if len(twins) > 0 {
					s.reportTwin(twins[0], ev, cost)
					return nil
				}
			}
			s.event(LevelInfo, "abs.unmatched", "", fmt.Sprintf("ABS %s for %s matches none of our recent orders: a purchase made outside this server, or an order too old", ev.Name, formatNaira(cost)), map[string]any{"abs_order": ev.OrderID})
			return nil
		case 1:
			match = &found[0]
		default:
			if settlesNothing(found, delivered) {
				// Say two identical airtime purchases both delivered: their messages fit both orders, but
				// neither order needs anything, so there is nothing to ask the owner.
				s.Log.Info("Webhook - ABS", "detail", fmt.Sprintf("ABS %s fits %d orders that are already in the state it reports; nothing to do", ev.Name, len(found)))
				return nil
			}
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
		if !delivered && match.Attempts > 1 && (match.Status == StatusPending || match.Status == StatusSuccess) {
			// The owner bought this order again, so a purchase failing says nothing certain about the order:
			// it may be the earlier purchase, and the later one may well have been delivered.
			if match.Status == StatusPending {
				s.event(LevelWarn, "abs.unclear", match.ID, "ABS reports a failed purchase for an order that was bought more than once, so which purchase it was is unknown: nothing was changed, check ABS's dashboard and use the resolve action", map[string]any{"abs_order": ev.OrderID})
			} else {
				s.Log.Info("Webhook - ABS", "detail", fmt.Sprintf("ABS reports a failed purchase for order %s, which was bought again and delivered: ignored", match.ID))
			}
			return nil
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
			summary(o)+"\nABS "+ev.OrderID+" says this purchase was delivered, but the order is "+string(o.Status)+". If a refund is still due, do not send it. Nothing was changed; check ABS's dashboard.\n"+
				"If this is a purchase you made by hand for this customer, close the order as delivered:\n  POST /admin/orders/{id}/resolve  {\"outcome\": \"delivered\", \"note\": \"bought by hand in the ABS dashboard\"}\n")
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

// absState is what a message says became of an ABS transaction, or "" for a message that is not about one.
func absState(ev abs.Event) string {
	switch {
	case ev.OrderID == "":
		return ""
	case ev.Name == "transaction.settled" && ev.Status == "1":
		return "settled"
	case ev.Name == "transaction.failed" && ev.Status == "3":
		return "failed"
	case ev.Name == "refund.issued":
		return "refunded"
	}
	return ""
}

// trackABS remembers the state ABS reports for one of its transactions. When a transaction ABS had said
// it failed or refunded now settles, the owner may have been charged for it again (and the customer may
// have received it twice, for instance if it was also bought by hand), so that is an error event and an
// email, and alerted comes back true. A message older than the one on record (they can arrive out of
// order) never changes the record.
func (s *Service) trackABS(ctx context.Context, ev abs.Event, state string) (alerted bool, err error) {
	at, perr := time.Parse(time.RFC3339, ev.CreatedAt)
	if perr != nil {
		at = time.Now()
	}
	prev, prevAt, err := s.Store.NoteABSState(ctx, ev.OrderID, state, at)
	if err != nil {
		return false, err
	}
	if state != "settled" || (prev != "failed" && prev != "refunded") || at.Before(prevAt) {
		return false, nil
	}
	s.event(LevelError, "abs.redelivered", "", fmt.Sprintf("ABS says a purchase it had %s has now settled, so you may have been charged again and the customer may have it twice (%s)", prev, formatNaira(koboOrZero(ev.Amount))),
		map[string]any{"abs_order": ev.OrderID, "was": prev})
	s.alert("abs-redelivered:"+ev.OrderID, refundThrottle, "ABS settled a purchase it had "+prev+": "+formatNaira(koboOrZero(ev.Amount)),
		"ABS purchase "+ev.OrderID+" was reported "+prev+" earlier, and ABS now reports it settled.\n\n"+
			"Recipient: "+ev.Recipient+"\nAmount ABS charged: "+formatNaira(koboOrZero(ev.Amount))+"\nABS's note: "+shortText(ev.Message, 200)+"\n\n"+
			"You may have been charged for it again, and the customer may have received it twice (for example if you also bought it by hand after the failure). "+
			"Nothing was changed on any order. Check the ABS dashboard and, if it is a double charge, ask ABS to reverse it, quoting the purchase id above.\n")
	return true, nil
}

// reportTwin: a second ABS purchase settled for the same recipient and cost as an order another ABS
// purchase already settled. Often a purchase made by hand on purpose, but it is also what a double
// delivery looks like, so the owner is told.
func (s *Service) reportTwin(o Order, ev abs.Event, cost int64) {
	s.event(LevelWarn, "abs.duplicate", o.ID, fmt.Sprintf("ABS settled a second purchase for %s that this order already has one for (%s), nothing was changed", formatNaira(cost), o.Status),
		map[string]any{"abs_order": ev.OrderID, "order_abs_ref": o.VendorRef})
	s.alert("abs-duplicate:"+ev.OrderID, refundThrottle, "Possible double purchase: "+formatNaira(cost)+" settled twice, order "+o.ID,
		summary(o)+"\nABS purchase "+ev.OrderID+" just settled for the same recipient and amount, and this order is already tied to ABS purchase "+o.VendorRef+" ("+string(o.Status)+").\n\n"+
			"If you bought it by hand on purpose, there is nothing to do. If not, ABS may have delivered and charged it twice: check the ABS dashboard and ask ABS to reverse the extra one, quoting the purchase id above. Nothing was changed on any order.\n")
}

func koboOrZero(amount string) int64 {
	k, _ := koboOf(amount)
	return k
}

// VendorTwins returns the orders from `since` for this recipient and cost that reached ABS and are already
// tied to a different ABS purchase than ref.
func (s *Store) VendorTwins(ctx context.Context, ref, recipient string, costKobo int64, since time.Time) ([]Order, error) {
	args := []any{ref, recipient, costKobo, since.Unix()}
	for _, st := range vendedStatuses {
		args = append(args, string(st))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE vendor_ref != '' AND vendor_ref != ? AND recipient = ? AND cost_kobo = ? AND created_at >= ?
			AND status IN (?`+strings.Repeat(",?", len(vendedStatuses)-1)+`)
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

// NoteABSState records the state ABS reports for one of its transactions, as of at, and returns the state
// and time on record before (empty if none). A report older than the record is not stored.
func (s *Store) NoteABSState(ctx context.Context, absID, state string, at time.Time) (prev string, prevAt time.Time, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	var prevUnix int64
	switch err := tx.QueryRowContext(ctx, `SELECT state, event_at FROM abs_transactions WHERE abs_id = ?`, absID).Scan(&prev, &prevUnix); {
	case errors.Is(err, sql.ErrNoRows):
		prev = ""
	case err != nil:
		return "", time.Time{}, err
	default:
		prevAt = time.Unix(prevUnix, 0)
		if at.Before(prevAt) {
			return prev, prevAt, nil
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO abs_transactions (abs_id, state, event_at, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(abs_id) DO UPDATE SET state = excluded.state, event_at = excluded.event_at, updated_at = excluded.updated_at`,
		absID, state, at.Unix(), time.Now().Unix()); err != nil {
		return "", time.Time{}, err
	}
	return prev, prevAt, tx.Commit()
}

// PurgeABSTransactions forgets what was recorded before the cutoff and says how many went.
func (s *Store) PurgeABSTransactions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM abs_transactions WHERE updated_at < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// settlesNothing says whether a message that fits several orders asks anything of the owner: not when every
// one of them is already in the state the message reports (delivered, or failed and refunded).
func settlesNothing(found []Order, delivered bool) bool {
	for _, o := range found {
		switch {
		case o.Status == StatusPending, o.Status == StatusVending:
			return false
		case delivered && o.Status != StatusSuccess:
			return false
		case !delivered && o.Status == StatusSuccess:
			return false
		}
	}
	return true
}
