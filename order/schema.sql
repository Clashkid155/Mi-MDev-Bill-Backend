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
    refund_amount_kobo INTEGER NOT NULL DEFAULT 0, -- what went back to the customer; 0 means all of it
    refund_note     TEXT NOT NULL DEFAULT '',
    refunded_at     INTEGER NOT NULL DEFAULT 0, -- unix seconds
    customer_email  TEXT NOT NULL,
    customer_name   TEXT NOT NULL DEFAULT '', -- given, or the email's local part; set once at creation
    network_name    TEXT NOT NULL DEFAULT '', -- display name of the network/DISCO as it was at purchase (a snapshot)
    plan_name       TEXT NOT NULL DEFAULT '', -- display name of the data plan as it was at purchase (a snapshot)
    plan_note       TEXT NOT NULL DEFAULT '', -- ABS's note on the plan at purchase (what the customer was told)
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

-- Money the business spends that no order records: hosting, email, the ABS agent
-- fee, a domain, the 50 naira duty on our own transfers. Entered by hand through
-- the admin API so /admin/stats can show a profit after expenses.
CREATE TABLE IF NOT EXISTS expenses (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    day         TEXT NOT NULL,                     -- the Nigerian day it belongs to, YYYY-MM-DD
    category    TEXT NOT NULL,                     -- hosting, email, agent_fee, domain, transfer_duty, other, ...
    label       TEXT NOT NULL,
    amount_kobo INTEGER NOT NULL CHECK (amount_kobo > 0),
    note        TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS expenses_day ON expenses (day);

-- Settings changed through the admin API. Each row overrides the value from .env
-- (or the built-in default); deleting the row puts the .env value back.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Every change to a setting: what it was in effect and what it became.
CREATE TABLE IF NOT EXISTS settings_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    key        TEXT NOT NULL,
    old_value  TEXT NOT NULL,
    new_value  TEXT NOT NULL,
    changed_at INTEGER NOT NULL
);
