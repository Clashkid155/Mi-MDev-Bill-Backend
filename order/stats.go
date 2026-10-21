package order

import (
	"context"
	"time"
)

// due is the SQL for what a customer paid for an order: the total, or for an
// order made before totals were kept, the amount.
const dueSQL = `(CASE WHEN total_kobo > 0 THEN total_kobo ELSE amount_kobo END)`

// A statuses where money was taken and no purchase was delivered.
const notDeliveredSQL = `('FAILED','REFUND_DUE','REFUNDED','EXPIRED','CANCELLED')`

// atRiskStatuses are orders where the customer's money is held and the outcome
// is still open or needs a person: the figure to watch every day.
var atRiskStatuses = []Status{StatusRefundDue, StatusPending, StatusVending, StatusPartiallyPaid, StatusOverpaid, StatusPaid}

// Stats is the business at a glance for a window of days. Money is in kobo.
//
// Profit counts only SUCCESS orders whose vendor cost was kept; older orders
// are counted in OrdersWithoutCost and left out, since a profit worked out
// without a cost would be invented. NetProfit also takes off the payment fees
// paid on orders that were never delivered (refunded, failed or expired after
// payment), because the provider keeps those whether or not the sale happened;
// whether a refund gives the fee back is not yet known, so it is counted as lost.
type Stats struct {
	From string `json:"from"` // first day, Nigerian time
	To   string `json:"to"`   // last day, Nigerian time

	Orders struct {
		Total       int            `json:"total"`
		ByStatus    map[Status]int `json:"by_status"`
		Succeeded   int            `json:"succeeded"`
		Undelivered int            `json:"undelivered"`  // FAILED, REFUND_DUE, REFUNDED
		FailureRate float64        `json:"failure_rate"` // undelivered / (succeeded + undelivered)
	} `json:"orders"`

	Money struct {
		RevenueKobo           int64   `json:"revenue_kobo"`      // paid by customers for delivered orders
		ServiceFeesKobo       int64   `json:"service_fees_kobo"` // part of the revenue
		VendorCostKobo        int64   `json:"vendor_cost_kobo"`
		PaymentFeesKobo       int64   `json:"payment_fees_kobo"`
		ProfitKobo            int64   `json:"profit_kobo"`
		MarginPercent         float64 `json:"margin_percent"` // profit / revenue, over orders with a known cost
		AverageOrderKobo      int64   `json:"average_order_kobo"`
		FeesOnUndeliveredKobo int64   `json:"payment_fees_on_undelivered_kobo"`
		RefundFeesKobo        int64   `json:"refund_fees_kobo"` // what sending refunds cost
		NetProfitKobo         int64   `json:"net_profit_kobo"`
		OrdersWithoutCost     int     `json:"orders_without_cost"`
	} `json:"money"`

	AtRisk struct {
		Count     int                   `json:"count"`
		TotalKobo int64                 `json:"total_kobo"`
		ByStatus  map[Status]RiskAmount `json:"by_status"`
	} `json:"at_risk"`

	ByKind    []Breakdown `json:"by_kind"`
	ByNetwork []Breakdown `json:"by_network"`
	TopPlans  []Breakdown `json:"top_plans"`
	Daily     []Day       `json:"daily"`
}

type RiskAmount struct {
	Count     int   `json:"count"`
	TotalKobo int64 `json:"total_kobo"`
}

// Breakdown is delivered orders grouped by something.
type Breakdown struct {
	Name        string `json:"name"`
	Orders      int    `json:"orders"`
	RevenueKobo int64  `json:"revenue_kobo"`
	ProfitKobo  int64  `json:"profit_kobo"`
}

// Day is one Nigerian day of delivered orders.
type Day struct {
	Date        string `json:"date"`
	Orders      int    `json:"orders"`
	RevenueKobo int64  `json:"revenue_kobo"`
	ProfitKobo  int64  `json:"profit_kobo"`
}

const dateLayout = "2006-01-02"

// Stats works out the figures for orders created from the start of day `from` to
// the end of day `to` (both Nigerian days, inclusive).
func (s *Store) Stats(ctx context.Context, from, to time.Time) (Stats, error) {
	from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, lagos)
	end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, lagos).AddDate(0, 0, 1)
	lo, hi := from.Unix(), end.Unix()

	var st Stats
	st.From, st.To = from.Format(dateLayout), end.AddDate(0, 0, -1).Format(dateLayout)
	st.Orders.ByStatus = map[Status]int{}
	st.AtRisk.ByStatus = map[Status]RiskAmount{}

	// Orders and money held, by status.
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*), COALESCE(SUM(`+dueSQL+`), 0) FROM orders
		WHERE created_at >= ? AND created_at < ? GROUP BY status`, lo, hi)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var status Status
		var n int
		var total int64
		if err := rows.Scan(&status, &n, &total); err != nil {
			rows.Close()
			return st, err
		}
		st.Orders.ByStatus[status] = n
		st.Orders.Total += n
		for _, risky := range atRiskStatuses {
			if status == risky {
				st.AtRisk.ByStatus[status] = RiskAmount{n, total}
				st.AtRisk.Count += n
				st.AtRisk.TotalKobo += total
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return st, err
	}
	rows.Close()
	st.Orders.Succeeded = st.Orders.ByStatus[StatusSuccess]
	st.Orders.Undelivered = st.Orders.ByStatus[StatusFailed] + st.Orders.ByStatus[StatusRefundDue] + st.Orders.ByStatus[StatusRefunded]
	if done := st.Orders.Succeeded + st.Orders.Undelivered; done > 0 {
		st.Orders.FailureRate = float64(st.Orders.Undelivered) / float64(done)
	}

	// Money, over delivered orders.
	var revenueKnown int64
	err = s.db.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(`+dueSQL+`), 0),
			COALESCE(SUM(fee_kobo), 0),
			COALESCE(SUM(CASE WHEN cost_kobo > 0 THEN cost_kobo END), 0),
			COALESCE(SUM(CASE WHEN cost_kobo > 0 THEN payment_fee_kobo END), 0),
			COALESCE(SUM(CASE WHEN cost_kobo > 0 THEN `+dueSQL+` - cost_kobo - payment_fee_kobo END), 0),
			COALESCE(SUM(CASE WHEN cost_kobo > 0 THEN `+dueSQL+` END), 0),
			COALESCE(SUM(CASE WHEN cost_kobo = 0 THEN 1 END), 0)
		FROM orders WHERE status = 'SUCCESS' AND created_at >= ? AND created_at < ?`, lo, hi).
		Scan(&st.Money.RevenueKobo, &st.Money.ServiceFeesKobo, &st.Money.VendorCostKobo, &st.Money.PaymentFeesKobo,
			&st.Money.ProfitKobo, &revenueKnown, &st.Money.OrdersWithoutCost)
	if err != nil {
		return st, err
	}
	if revenueKnown > 0 {
		st.Money.MarginPercent = float64(st.Money.ProfitKobo) / float64(revenueKnown) * 100
	}
	if st.Orders.Succeeded > 0 {
		st.Money.AverageOrderKobo = st.Money.RevenueKobo / int64(st.Orders.Succeeded)
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(payment_fee_kobo), 0), COALESCE(SUM(refund_fee_kobo), 0) FROM orders
		WHERE status IN `+notDeliveredSQL+` AND created_at >= ? AND created_at < ?`, lo, hi).
		Scan(&st.Money.FeesOnUndeliveredKobo, &st.Money.RefundFeesKobo)
	if err != nil {
		return st, err
	}
	st.Money.NetProfitKobo = st.Money.ProfitKobo - st.Money.FeesOnUndeliveredKobo - st.Money.RefundFeesKobo

	// Delivered orders, grouped.
	group := func(column, label string, limit int, orderBy string) ([]Breakdown, error) {
		rows, err := s.db.QueryContext(ctx, `SELECT `+column+`, COUNT(*), COALESCE(SUM(`+dueSQL+`), 0),
				COALESCE(SUM(CASE WHEN cost_kobo > 0 THEN `+dueSQL+` - cost_kobo - payment_fee_kobo END), 0)
			FROM orders WHERE status = 'SUCCESS' AND created_at >= ? AND created_at < ?`+label+`
			GROUP BY `+column+` ORDER BY `+orderBy+` LIMIT ?`, lo, hi, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []Breakdown{}
		for rows.Next() {
			var b Breakdown
			if err := rows.Scan(&b.Name, &b.Orders, &b.RevenueKobo, &b.ProfitKobo); err != nil {
				return nil, err
			}
			if b.Name == "" {
				b.Name = "(not recorded)"
			}
			out = append(out, b)
		}
		return out, rows.Err()
	}
	if st.ByKind, err = group("kind", "", 10, "2 DESC"); err != nil {
		return st, err
	}
	if st.ByNetwork, err = group("network_name", "", 10, "3 DESC"); err != nil {
		return st, err
	}
	if st.TopPlans, err = group("plan_name", ` AND plan_id != ''`, 10, "2 DESC, 3 DESC"); err != nil {
		return st, err
	}

	// A row for every day in the window, including days with no sales.
	days := map[string]Day{}
	rows, err = s.db.QueryContext(ctx, `SELECT strftime('%Y-%m-%d', created_at, 'unixepoch', '+1 hours'), COUNT(*),
			COALESCE(SUM(`+dueSQL+`), 0), COALESCE(SUM(CASE WHEN cost_kobo > 0 THEN `+dueSQL+` - cost_kobo - payment_fee_kobo END), 0)
		FROM orders WHERE status = 'SUCCESS' AND created_at >= ? AND created_at < ? GROUP BY 1`, lo, hi)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.Date, &d.Orders, &d.RevenueKobo, &d.ProfitKobo); err != nil {
			rows.Close()
			return st, err
		}
		days[d.Date] = d
	}
	rows.Close()
	for day := from; day.Before(end); day = day.AddDate(0, 0, 1) {
		key := day.Format(dateLayout)
		d, ok := days[key]
		if !ok {
			d = Day{Date: key}
		}
		st.Daily = append(st.Daily, d)
	}
	return st, nil
}
