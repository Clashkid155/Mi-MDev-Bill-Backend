package order

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Order struct {
	ID             string
	IdempotencyKey string
	Kind           string
	Network        string
	PlanID         string
	Recipient      string
	AmountKobo     int64 // the value bought: what the vendor is asked to deliver
	// CostKobo is what the vendor charges us for it, FeeKobo the service fee the
	// customer pays on top (electricity only) and TotalKobo what the customer
	// pays in all. PaymentFeeKobo is what the payment provider took, known once
	// paid. Orders made before these were kept have them all as 0; see Due.
	CostKobo       int64
	FeeKobo        int64
	TotalKobo      int64
	PaymentFeeKobo int64
	// Set when REFUND_DUE is closed by MarkRefunded: what the refund cost us, a note
	// about it, and when it was done.
	RefundFeeKobo int64
	// RefundAmountKobo is what was actually sent back to the customer. 0 means all of
	// it (the whole of Due), also for refunds recorded before this was kept. A smaller
	// amount means the costs of the transaction were deducted.
	RefundAmountKobo int64
	RefundNote       string
	RefundedAt       time.Time
	CustomerEmail    string
	CustomerName     string
	NetworkName      string // snapshot of the catalog's display name at purchase; ABS renames things
	PlanName         string // same, for data plans
	// PlanNote is ABS's note on the plan as it was at purchase (a fair use limit,
	// "won't credit if the line owes airtime"): what the customer was told.
	PlanNote     string
	Status       Status
	InvoiceRef   string
	TxnRef       string
	AccountNo    string
	AccountName  string
	BankName     string
	CheckoutURL  string
	ExpiresAt    time.Time
	VendResponse string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Due is what the customer has to pay: the total, or for an order made before
// totals were kept, the amount.
func (o Order) Due() int64 {
	if o.TotalKobo > 0 {
		return o.TotalKobo
	}
	return o.AmountKobo
}

// Refunded is how much of a REFUNDED order's money went back to the customer: what
// was recorded, or all of it when nothing smaller was recorded.
func (o Order) Refunded() int64 {
	if o.Status != StatusRefunded {
		return 0
	}
	if o.RefundAmountKobo > 0 {
		return o.RefundAmountKobo
	}
	return o.Due()
}

// Profit is what a SUCCESS order earned: what the customer paid, less what the
// vendor charged and what the payment provider took. It is nil for any other
// status, and for an order made before costs were kept (its cost is 0), because
// a profit worked out without a cost would be invented.
func (o Order) Profit() *int64 {
	if o.Status != StatusSuccess || o.CostKobo <= 0 {
		return nil
	}
	p := o.Due() - o.CostKobo - o.PaymentFeeKobo
	return &p
}

var ErrNotFound = errors.New("order: not found")

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := keepPrivate(path); err != nil {
		return nil, err
	}
	// foreign_keys: nothing refers to anything yet, but a reference added later
	// should be enforced from its first day.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite allows one writer; this avoids "database is locked"
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Refuse to run on a damaged file: better a stopped server than silently wrong money.
	var check string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
		db.Close()
		return nil, fmt.Errorf("the database file %s is damaged (restore the latest backup): %v %s", path, err, check)
	}
	// Databases created before these columns existed need them added.
	for _, column := range []string{"plan_id", "customer_name", "network_name", "plan_name", "plan_note"} {
		if err := addColumnIfMissing(db, "orders", column, "TEXT NOT NULL DEFAULT ''"); err != nil {
			db.Close()
			return nil, err
		}
	}
	for _, column := range []string{"cost_kobo", "fee_kobo", "total_kobo", "payment_fee_kobo", "refund_fee_kobo", "refund_amount_kobo", "refunded_at"} {
		if err := addColumnIfMissing(db, "orders", column, "INTEGER NOT NULL DEFAULT 0"); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := addColumnIfMissing(db, "orders", "refund_note", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	// SQLite gives the -wal and -shm files the main file's permissions; make sure
	// the main file is the private one it was above.
	_ = keepPrivate(path)
	return &Store{db: db}, nil
}

// keepPrivate makes the database file readable by its owner only: it holds
// customers' emails and phone numbers and the electricity tokens we sold. A
// missing file is created empty with those permissions first, so there is never
// a moment when it is open to others.
func keepPrivate(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// idAlphabet is Crockford's base32: digits and capitals without I, L, O and U,
// so an id read out over the phone or copied by hand cannot be mistaken.
const idAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID is our own order id, and doubles as the reference we give the payment
// provider, so it appears verbatim in their dashboard and webhooks. "MDEV_"
// marks it as ours, the way Monnify's own generated transactionReference is
// marked "MNFY|...". The rest is 16 characters, each picked from 32 symbols by
// 5 random bits (80 bits in all), so ids are short enough to read and type yet
// impossible to guess, which matters because anyone holding an id can poll its
// order. Orders made before this change keep their older ids; nothing depends
// on an id's length.
//
// Uniqueness does not rest on luck: the id is the table's primary key, so a
// repeat can never be stored, and Insert draws a fresh id if one ever clashes.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = idAlphabet[v&31] // 256 is a multiple of 32, so every symbol is equally likely
	}
	return "MDEV_" + string(out)
}

// idGen makes order ids; tests replace it to force a clash.
var idGen = newID

const orderColumns = `id, idempotency_key, kind, network, plan_id, recipient, amount_kobo, cost_kobo, fee_kobo, total_kobo, payment_fee_kobo, refund_fee_kobo, refund_amount_kobo, refund_note, refunded_at, customer_email, customer_name, network_name, plan_name, plan_note, status,
	invoice_ref, txn_ref, account_no, account_name, bank_name, checkout_url, expires_at, vend_response,
	created_at, updated_at`

func scanOrder(row interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var status string
	var expires, created, updated, refunded int64
	err := row.Scan(&o.ID, &o.IdempotencyKey, &o.Kind, &o.Network, &o.PlanID, &o.Recipient, &o.AmountKobo, &o.CostKobo, &o.FeeKobo, &o.TotalKobo, &o.PaymentFeeKobo, &o.RefundFeeKobo, &o.RefundAmountKobo, &o.RefundNote, &refunded, &o.CustomerEmail,
		&o.CustomerName, &o.NetworkName, &o.PlanName, &o.PlanNote, &status, &o.InvoiceRef, &o.TxnRef, &o.AccountNo, &o.AccountName, &o.BankName, &o.CheckoutURL,
		&expires, &o.VendResponse, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.Status = Status(status)
	o.ExpiresAt = time.Unix(expires, 0)
	o.CreatedAt = time.Unix(created, 0)
	o.UpdatedAt = time.Unix(updated, 0)
	if refunded > 0 {
		o.RefundedAt = time.Unix(refunded, 0)
	}
	return o, err
}

func (s *Store) ByID(ctx context.Context, id string) (Order, error) {
	return scanOrder(s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = ?`, id))
}

func (s *Store) ByIdempotencyKey(ctx context.Context, key string) (Order, error) {
	return scanOrder(s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE idempotency_key = ?`, key))
}

func (s *Store) ByTxnRef(ctx context.Context, txnRef string) (Order, error) {
	if txnRef == "" {
		return Order{}, ErrNotFound
	}
	return scanOrder(s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE txn_ref = ?`, txnRef))
}

// List returns orders, newest first, optionally only those in the given statuses.
func (s *Store) List(ctx context.Context, statuses []Status, limit int) ([]Order, error) {
	query, args := `SELECT `+orderColumns+` FROM orders`, []any{}
	if len(statuses) > 0 {
		query += ` WHERE status IN (?` + strings.Repeat(",?", len(statuses)-1) + `)`
		for _, st := range statuses {
			args = append(args, string(st))
		}
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at DESC, id LIMIT ?`, append(args, limit)...)
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

// AwaitingPastExpiry returns orders still waiting for payment whose payment
// window ended before `now`, oldest first.
func (s *Store) AwaitingPastExpiry(ctx context.Context, now time.Time, limit int) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE status = ? AND expires_at < ?
		ORDER BY expires_at LIMIT ?`, StatusAwaitingPayment, now.Unix(), limit)
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

// Insert stores a new INITIATED order and fills in its ID and timestamps.
func (s *Store) Insert(ctx context.Context, o *Order) error {
	now := time.Now()
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		o.ID, o.Status, o.CreatedAt, o.UpdatedAt = idGen(), StatusInitiated, now, now
		_, err = s.db.ExecContext(ctx, `INSERT INTO orders
		(id, idempotency_key, kind, network, plan_id, recipient, amount_kobo, cost_kobo, fee_kobo, total_kobo, customer_email, customer_name, network_name, plan_name, plan_note, status, expires_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			o.ID, o.IdempotencyKey, o.Kind, o.Network, o.PlanID, o.Recipient, o.AmountKobo, o.CostKobo, o.FeeKobo, o.TotalKobo, o.CustomerEmail, o.CustomerName, o.NetworkName, o.PlanName, o.PlanNote, o.Status,
			o.ExpiresAt.Unix(), now.Unix(), now.Unix())
		// Only a clash on the id itself is worth another try; a repeated
		// idempotency key is the caller's to handle.
		if err == nil || !idClash(err) {
			return err
		}
	}
	return err
}

// ErrNotRefundable means an order is not waiting for a refund.
var ErrNotRefundable = errors.New("order: not waiting for a refund")

// MarkRefunded closes an order that was waiting for a refund (REFUND_DUE) once the
// money has been sent back by hand. refundFeeKobo is what sending it cost us (a
// refund fee), note is for the record. It is compare-and-set, so a second call
// cannot overwrite the first. The order is returned as it is afterwards; for an
// order in any other status, ErrNotRefundable comes back with the order as it is.
//
// feeReturned says the payment provider gave back its fee on the original payment
// (then it is no longer a cost, and the order's payment fee becomes 0).
//
// refundAmountKobo is what was sent back to the customer: 0 for all of it, or less
// when the transaction's costs were deducted.
func (s *Store) MarkRefunded(ctx context.Context, id string, refundFeeKobo, refundAmountKobo int64, note string, feeReturned bool) (Order, error) {
	if err := Check(StatusRefundDue, StatusRefunded); err != nil {
		return Order{}, err
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET status = ?, refund_fee_kobo = ?, refund_amount_kobo = ?, refund_note = ?, refunded_at = ?, updated_at = ?,
			payment_fee_kobo = CASE WHEN ? THEN 0 ELSE payment_fee_kobo END
		WHERE id = ? AND status = ?`, StatusRefunded, refundFeeKobo, refundAmountKobo, note, now, now, feeReturned, id, StatusRefundDue)
	if err != nil {
		return Order{}, err
	}
	o, err := s.ByID(ctx, id)
	if err != nil {
		return Order{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return o, ErrNotRefundable
	}
	return o, nil
}

// SetPaymentFee records what the payment provider took for an order.
func (s *Store) SetPaymentFee(ctx context.Context, id string, kobo int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE orders SET payment_fee_kobo = ?, updated_at = ? WHERE id = ?`, kobo, time.Now().Unix(), id)
	return err
}

// idClash reports whether err is SQLite refusing a repeated order id. The
// message names the column ("UNIQUE constraint failed: orders.id"), and
// orders.idempotency_key must not be mistaken for it.
func idClash(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed: orders.id") && !strings.Contains(msg, "orders.idempotency_key")
}

// AttachInvoice records the Monnify invoice and moves INITIATED -> AWAITING_PAYMENT.
func (s *Store) AttachInvoice(ctx context.Context, o *Order) error {
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET status = ?, invoice_ref = ?, txn_ref = ?, account_no = ?,
		account_name = ?, bank_name = ?, checkout_url = ?, expires_at = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		StatusAwaitingPayment, o.InvoiceRef, o.TxnRef, o.AccountNo, o.AccountName, o.BankName, o.CheckoutURL,
		o.ExpiresAt.Unix(), time.Now().Unix(), o.ID, StatusInitiated)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("order: not in INITIATED state")
	}
	o.Status = StatusAwaitingPayment
	return nil
}

// Move changes an order's status only if it is currently `from` (compare-and-set),
// so duplicate webhooks and concurrent workers cannot both win. moved is false
// when someone else already changed it.
func (s *Store) Move(ctx context.Context, id string, from, to Status) (moved bool, err error) {
	if err := Check(from, to); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, time.Now().Unix(), id, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MoveWithVendResponse is Move that also stores the raw ABS reply.
func (s *Store) MoveWithVendResponse(ctx context.Context, id string, from, to Status, vendResponse string) (bool, error) {
	if err := Check(from, to); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET status = ?, vend_response = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, vendResponse, time.Now().Unix(), id, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

type WebhookEvent struct {
	ID         int64
	Provider   string
	EventKey   string
	Body       string
	ReceivedAt time.Time
}

// Webhooks returns the saved webhooks, newest first, optionally only one provider's.
func (s *Store) Webhooks(ctx context.Context, provider string, limit int) ([]WebhookEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, provider, event_key, body, received_at FROM webhook_events
		WHERE ? = '' OR provider = ? ORDER BY id DESC LIMIT ?`, provider, provider, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []WebhookEvent
	for rows.Next() {
		var e WebhookEvent
		var received int64
		if err := rows.Scan(&e.ID, &e.Provider, &e.EventKey, &e.Body, &received); err != nil {
			return nil, err
		}
		e.ReceivedAt = time.Unix(received, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}

// PurgeWebhooks deletes the raw webhook bodies received before the cutoff and
// says how many went. They are an audit trail, not the record of any sale (the
// order is), so they do not need to be kept forever.
func (s *Store) PurgeWebhooks(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhook_events WHERE received_at < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) LogWebhook(ctx context.Context, provider, eventKey string, body []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhook_events (provider, event_key, body, received_at) VALUES (?,?,?,?)`,
		provider, eventKey, string(body), time.Now().Unix())
	return err
}
