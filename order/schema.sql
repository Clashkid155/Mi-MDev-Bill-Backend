CREATE TABLE IF NOT EXISTS orders (
    id              TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    kind            TEXT NOT NULL,
    network         TEXT NOT NULL,             -- ABS network / disco id
    plan_id         TEXT NOT NULL DEFAULT '',   -- data plan id
    recipient       TEXT NOT NULL,             -- phone, FibreX FN or meter number
    amount_kobo     INTEGER NOT NULL CHECK (amount_kobo > 0),  -- the value bought
    cost_kobo       INTEGER NOT NULL DEFAULT 0, -- what the vendor charges us for it (0 on orders from before this was kept)
    fee_kobo        INTEGER NOT NULL DEFAULT 0, -- service fee charged to the customer on top (electricity)
    total_kobo      INTEGER NOT NULL DEFAULT 0, -- what the customer pays: amount + fee (0 on old orders: they paid the amount)
    payment_fee_kobo INTEGER NOT NULL DEFAULT 0, -- what the payment provider took, known once paid
    refund_fee_kobo INTEGER NOT NULL DEFAULT 0, -- what sending a refund cost us, set when a REFUND_DUE order is marked refunded
    refund_note     TEXT NOT NULL DEFAULT '',
    refunded_at     INTEGER NOT NULL DEFAULT 0, -- unix seconds
    customer_email  TEXT NOT NULL,
    customer_name   TEXT NOT NULL DEFAULT '', -- given, or the email's local part; set once at creation
    network_name    TEXT NOT NULL DEFAULT '', -- display name of the network/DISCO as it was at purchase (a snapshot)
    plan_name       TEXT NOT NULL DEFAULT '', -- display name of the data plan as it was at purchase (a snapshot)
    status          TEXT NOT NULL,
    invoice_ref     TEXT NOT NULL DEFAULT '',
    txn_ref         TEXT NOT NULL DEFAULT '',
    account_no      TEXT NOT NULL DEFAULT '',
    account_name    TEXT NOT NULL DEFAULT '',
    bank_name       TEXT NOT NULL DEFAULT '',
    checkout_url    TEXT NOT NULL DEFAULT '',
    expires_at      INTEGER NOT NULL,          -- unix seconds
    vend_response   TEXT NOT NULL DEFAULT '',  -- raw ABS reply; may hold tokens/pins
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS orders_txn_ref ON orders (txn_ref);
CREATE INDEX IF NOT EXISTS orders_status ON orders (status, updated_at);

-- Audit log of every webhook received, from either provider.
CREATE TABLE IF NOT EXISTS webhook_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    provider    TEXT NOT NULL,
    event_key   TEXT NOT NULL,
    body        TEXT NOT NULL,
    received_at INTEGER NOT NULL
);
