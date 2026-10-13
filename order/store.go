package order

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
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
	AmountKobo     int64
	CustomerEmail  string
	CustomerName   string
	NetworkName    string // snapshot of the catalog's display name at purchase; ABS renames things
	PlanName       string // same, for data plans
	Status         Status
	InvoiceRef     string
	TxnRef         string
	AccountNo      string
	AccountName    string
	BankName       string
	CheckoutURL    string
	ExpiresAt      time.Time
	VendResponse   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

var ErrNotFound = errors.New("order: not found")

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite allows one writer; this avoids "database is locked"
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Databases created before these columns existed need them added.
	for _, column := range []string{"plan_id", "customer_name", "network_name", "plan_name"} {
		if err := addColumnIfMissing(db, "orders", column, "TEXT NOT NULL DEFAULT ''"); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
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

// newID is our own order id, and doubles as the invoiceReference /
// paymentReference we give Monnify, so it appears verbatim in their dashboard
// and webhooks. "MDEV_" marks it as ours, the way Monnify's own generated
// transactionReference is marked "MNFY|...".
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "MDEV_" + hex.EncodeToString(b)
}

const orderColumns = `id, idempotency_key, kind, network, plan_id, recipient, amount_kobo, customer_email, customer_name, network_name, plan_name, status,
	invoice_ref, txn_ref, account_no, account_name, bank_name, checkout_url, expires_at, vend_response,
	created_at, updated_at`

func scanOrder(row interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var status string
	var expires, created, updated int64
	err := row.Scan(&o.ID, &o.IdempotencyKey, &o.Kind, &o.Network, &o.PlanID, &o.Recipient, &o.AmountKobo, &o.CustomerEmail,
		&o.CustomerName, &o.NetworkName, &o.PlanName, &status, &o.InvoiceRef, &o.TxnRef, &o.AccountNo, &o.AccountName, &o.BankName, &o.CheckoutURL,
		&expires, &o.VendResponse, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.Status = Status(status)
	o.ExpiresAt = time.Unix(expires, 0)
	o.CreatedAt = time.Unix(created, 0)
	o.UpdatedAt = time.Unix(updated, 0)
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
	o.ID, o.Status, o.CreatedAt, o.UpdatedAt = newID(), StatusInitiated, now, now
	_, err := s.db.ExecContext(ctx, `INSERT INTO orders
		(id, idempotency_key, kind, network, plan_id, recipient, amount_kobo, customer_email, customer_name, network_name, plan_name, status, expires_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.IdempotencyKey, o.Kind, o.Network, o.PlanID, o.Recipient, o.AmountKobo, o.CustomerEmail, o.CustomerName, o.NetworkName, o.PlanName, o.Status,
		o.ExpiresAt.Unix(), now.Unix(), now.Unix())
	return err
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

func (s *Store) LogWebhook(ctx context.Context, provider, eventKey string, body []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhook_events (provider, event_key, body, received_at) VALUES (?,?,?,?)`,
		provider, eventKey, string(body), time.Now().Unix())
	return err
}
