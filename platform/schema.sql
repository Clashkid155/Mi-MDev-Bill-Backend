-- Tables the whole backend shares, whichever app or feature uses it: admin tokens, the audit log and events.

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

-- How the outside services (payment provider, ABS, mail) answered our calls, per provider and hour.
-- Written by ProviderHealth once a minute and kept for 90 days.
CREATE TABLE IF NOT EXISTS provider_stats (
    provider      TEXT NOT NULL,
    hour          INTEGER NOT NULL,           -- unix seconds, the start of the hour
    calls         INTEGER NOT NULL DEFAULT 0,
    failures      INTEGER NOT NULL DEFAULT 0, -- no answer, 5xx or 429
    slow          INTEGER NOT NULL DEFAULT 0, -- answered, but after 5 seconds or more
    total_ms      INTEGER NOT NULL DEFAULT 0, -- summed, for the average
    last_error    TEXT NOT NULL DEFAULT '',   -- the last failure in this hour, redacted
    last_error_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider, hour)
) WITHOUT ROWID;
