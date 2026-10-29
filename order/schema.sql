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

-- Named admin tokens, one per person or device, so each can be told apart in the audit log and
-- revoked on its own. Only a hash of the token is kept: the token itself is shown once, when it is
-- made. (The ADMIN_TOKEN in the settings file is the owner token and is not stored here.)
CREATE TABLE IF NOT EXISTS admin_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    token_hash   TEXT NOT NULL UNIQUE,   -- sha256 of the token, hex
    hint         TEXT NOT NULL,          -- the last 4 characters, to tell tokens apart in a list
    created_at   INTEGER NOT NULL,
    created_by   TEXT NOT NULL,
    last_used_at INTEGER NOT NULL DEFAULT 0,
    last_used_ip TEXT NOT NULL DEFAULT '',
    revoked_at   INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS admin_tokens_active_name ON admin_tokens (name) WHERE revoked_at = 0;

-- Everything an admin changed: who (token name), from where, what, and what happened. Reads are
-- not logged (too many); every write is, as is an address being locked out for guessing a token.
CREATE TABLE IF NOT EXISTS audit_log (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    at       INTEGER NOT NULL,
    actor    TEXT NOT NULL,                -- the token's name, "owner", or "(unknown)" for a lockout
    actor_id INTEGER NOT NULL DEFAULT 0,   -- admin_tokens.id, 0 for the owner
    role     TEXT NOT NULL DEFAULT '',
    ip       TEXT NOT NULL DEFAULT '',
    method   TEXT NOT NULL DEFAULT '',
    route    TEXT NOT NULL DEFAULT '',     -- the route, e.g. POST /admin/orders/{id}/refunded
    path     TEXT NOT NULL DEFAULT '',     -- the real path, which holds the order id
    status   INTEGER NOT NULL DEFAULT 0,
    detail   TEXT NOT NULL DEFAULT ''      -- the request body with anything secret removed, or a note
);
CREATE INDEX IF NOT EXISTS audit_log_at ON audit_log (at);
CREATE INDEX IF NOT EXISTS audit_log_actor ON audit_log (actor, id);

-- What happened, in words an owner can read: orders created, paid, delivered or failed, refunds, and the
-- warnings and errors the server logged. A small, redacted record (no phone numbers, emails, meter numbers
-- or tokens) kept for EVENT_RETENTION_DAYS, as opposed to the raw log in the journal.
CREATE TABLE IF NOT EXISTS events (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    at       INTEGER NOT NULL,
    level    TEXT NOT NULL CHECK (level IN ('info', 'warn', 'error')),
    kind     TEXT NOT NULL,                -- order.paid, refund.due, system, ...
    order_id TEXT NOT NULL DEFAULT '',
    message  TEXT NOT NULL,
    detail   TEXT NOT NULL DEFAULT ''      -- a small JSON object of figures, or empty
);
CREATE INDEX IF NOT EXISTS events_at ON events (at);
CREATE INDEX IF NOT EXISTS events_order ON events (order_id, id);
CREATE INDEX IF NOT EXISTS events_level ON events (level, id);
